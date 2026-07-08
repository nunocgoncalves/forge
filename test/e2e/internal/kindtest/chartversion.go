package kindtest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// This file implements auto-resolution of the control-plane Helm chart version
// and image tag for the e2e (HOR-321). Previously both were hardcoded defaults
// that drifted from every control-plane service release, forcing a manual forge
// bump. Now:
//
//   - the chart version is resolved from the charts repo's GitHub releases
//     (highest stable <chart>-<semver> tag), and
//   - the image tag is derived from the chart's appVersion, so the deployed
//     image can never drift from the chart (the control-plane chart keeps
//     appVersion == service version == image tag, per HOR-317).
//
// CONTROL_PLANE_CHART_VERSION / CONTROL_PLANE_IMAGE_TAG remain as explicit
// overrides for pinning or local dev.

// chartsGitHubRepo returns the "owner/name" GitHub repo that publishes forge's
// Helm charts as git tags of the form "<chart>-<semver>" (e.g.
// "control-plane-0.2.1"), created by chart-releaser. Defaults to the public
// nunocgoncalves/iterabase-charts repo; override with FORGE_CHARTS_REPO for forks.
func chartsGitHubRepo() string {
	if r := os.Getenv("FORGE_CHARTS_REPO"); r != "" {
		return r
	}
	return "nunocgoncalves/iterabase-charts"
}

// LatestChartVersion resolves the highest stable semver published for the named
// chart by listing GitHub releases on the charts repo and filtering tags of the
// form "<chart>-<semver>". Prereleases and drafts are skipped so PR-time CI
// tracks stable service releases (chart-releaser marks a release prerelease when
// the chart version carries a -prerelease suffix).
//
// It authenticates with GITHUB_TOKEN when present (CI has it; raises the rate
// limit from 60 to 5000 req/hour) and falls back to unauthenticated access
// otherwise. On failure it fails the test with a message pointing at
// CONTROL_PLANE_CHART_VERSION as the manual pin escape hatch.
func LatestChartVersion(t *testing.T, chart string) string {
	t.Helper()
	repo := chartsGitHubRepo()
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases?per_page=100", repo)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build github releases request for %s: %v\n"+
			"set CONTROL_PLANE_CHART_VERSION to pin a chart version and skip auto-resolution.", repo, err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("github releases request for %s: %v\n"+
			"set CONTROL_PLANE_CHART_VERSION to pin a chart version and skip auto-resolution.", repo, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("github releases request for %s: status %d\n"+
			"set CONTROL_PLANE_CHART_VERSION to pin a chart version and skip auto-resolution.",
			repo, resp.StatusCode)
	}
	var releases []struct {
		TagName    string `json:"tag_name"`
		Prerelease bool   `json:"prerelease"`
		Draft      bool   `json:"draft"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&releases); err != nil {
		t.Fatalf("decode github releases for %s: %v", repo, err)
	}
	prefix := chart + "-"
	var best, bestTag string
	for _, r := range releases {
		if r.Draft || r.Prerelease {
			continue
		}
		if !strings.HasPrefix(r.TagName, prefix) {
			continue
		}
		ver := strings.TrimPrefix(r.TagName, prefix)
		if !looksSemver(ver) {
			continue
		}
		if best == "" || compareSemver(ver, best) > 0 {
			best = ver
			bestTag = r.TagName
		}
	}
	if best == "" {
		t.Fatalf("no stable %q release tags found in %s (scanned %d releases)\n"+
			"set CONTROL_PLANE_CHART_VERSION to pin a chart version.", chart, repo, len(releases))
	}
	t.Logf("resolved latest %s chart version: %s (from tag %s)", chart, best, bestTag)
	return best
}

// ChartAppVersion returns the appVersion declared by the chart's metadata,
// deriving the image tag from the chart so the deployed image can never drift
// from it. For a local chart path it runs `helm show chart <path>`; for a
// remote/OCI chart it runs `helm show chart <ref> --version <v>`.
func ChartAppVersion(t *testing.T, chartRef, version, localChart string) string {
	t.Helper()
	mustBin(t, "helm")
	args := []string{"show", "chart"}
	if localChart != "" {
		args = append(args, localChart)
	} else {
		args = append(args, chartRef)
		if version != "" {
			args = append(args, "--version", version)
		}
	}
	out := run(t, "helm", args...)
	appVer := parseAppVersion(out)
	if appVer == "" {
		t.Fatalf("could not find appVersion in `helm show chart` output:\n%s", out)
	}
	t.Logf("derived image tag from chart appVersion: %s", appVer)
	return appVer
}

// appVersionLineRe matches the top-level appVersion line of `helm show chart`
// (Chart.yaml) output. Top-level keys have no leading indentation, which keeps
// this from matching a nested field of the same name.
var appVersionLineRe = regexp.MustCompile(`(?m)^appVersion:\s*(.*)$`)

// parseAppVersion extracts the top-level appVersion scalar from Chart.yaml
// output, stripping surrounding quotes and any trailing inline comment.
func parseAppVersion(chartYAML string) string {
	m := appVersionLineRe.FindStringSubmatch(chartYAML)
	if m == nil {
		return ""
	}
	v := m[1]
	// strip an inline comment: ` # ...` (a # only starts a comment when preceded
	// by whitespace or at line start; a scalar version contains no #).
	if i := strings.Index(v, " #"); i >= 0 {
		v = v[:i]
	}
	v = strings.TrimSpace(v)
	if len(v) >= 2 {
		first, last := v[0], v[len(v)-1]
		if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
			v = v[1 : len(v)-1]
		}
	}
	return v
}

// looksSemver reports whether v looks like a semver MAJOR.MINOR.PATCH core
// (with an optional leading "v" and optional -prerelease/+build suffix). It is a
// light pre-filter so non-version tags are ignored.
func looksSemver(v string) bool {
	core, _ := splitSemver(v)
	parts := strings.Split(core, ".")
	if len(parts) < 2 || len(parts) > 3 {
		return false
	}
	for _, p := range parts {
		if p == "" {
			return false
		}
		if _, err := strconv.Atoi(p); err != nil {
			return false
		}
	}
	return true
}

// compareSemver compares two semver strings (optional leading "v", optional
// -prerelease/+build). Returns -1, 0, or 1. A version without a prerelease ranks
// higher than one with a prerelease at the same MAJOR.MINOR.PATCH; prereleases
// are compared lexically (sufficient for the filtered stable set we keep).
func compareSemver(a, b string) int {
	acore, apre := splitSemver(a)
	bcore, bpre := splitSemver(b)
	if c := cmpNumParts(acore, bcore); c != 0 {
		return c
	}
	switch {
	case apre == "" && bpre != "":
		return 1
	case apre != "" && bpre == "":
		return -1
	case apre == bpre:
		return 0
	case apre < bpre:
		return -1
	default:
		return 1
	}
}

// splitSemver returns (core "MAJOR.MINOR.PATCH", prerelease) for a semver
// string, stripping a leading "v" and any +build metadata.
func splitSemver(v string) (core, pre string) {
	v = strings.TrimPrefix(v, "v")
	if i := strings.Index(v, "+"); i >= 0 {
		v = v[:i]
	}
	if i := strings.Index(v, "-"); i >= 0 {
		return v[:i], v[i+1:]
	}
	return v, ""
}

// cmpNumParts compares two dot-joined numeric version cores field by field,
// treating a missing trailing field as 0 (so "1.2" == "1.2.0").
func cmpNumParts(a, b string) int {
	ap := strings.Split(a, ".")
	bp := strings.Split(b, ".")
	n := len(ap)
	if len(bp) > n {
		n = len(bp)
	}
	for i := 0; i < n; i++ {
		var an, bn int
		if i < len(ap) {
			an, _ = strconv.Atoi(ap[i])
		}
		if i < len(bp) {
			bn, _ = strconv.Atoi(bp[i])
		}
		if an < bn {
			return -1
		}
		if an > bn {
			return 1
		}
	}
	return 0
}
