package kindtest

import (
	"strings"
	"testing"
)

// Unit tests for the pure helpers behind chart auto-resolution (HOR-321). The
// network/helm-backed LatestChartVersion and ChartAppVersion are exercised by
// the control-plane e2e itself; these cover the parsing/comparison logic that
// decides which release wins. Run with: make test-e2e-unit.

func TestParseAppVersion(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{"bare", "apiVersion: v2\nappVersion: 0.0.2\n", "0.0.2"},
		{"double-quoted", "apiVersion: v2\nappVersion: \"0.0.2\"\n", "0.0.2"},
		{"single-quoted", "apiVersion: v2\nappVersion: '0.0.2'\n", "0.0.2"},
		{"leading-v", "appVersion: v0.0.2\n", "v0.0.2"},
		{"inline-comment", "appVersion: 0.0.2 # service version\n", "0.0.2"},
		{"quoted-inline-comment", "appVersion: \"0.0.2\" # svc\n", "0.0.2"},
		{"trailing-spaces", "appVersion: 0.0.2   \n", "0.0.2"},
		{"missing", "apiVersion: v2\nname: control-plane\n", ""},
		{"nested-not-matched", "appVersion: top\nsub:\n  appVersion: 1.2.3\n", "top"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseAppVersion(c.yaml); got != c.want {
				t.Errorf("parseAppVersion(%q) = %q, want %q", c.yaml, got, c.want)
			}
		})
	}
}

func TestLooksSemver(t *testing.T) {
	good := []string{"0.2.1", "v0.2.1", "1.0.0", "10.20.30", "0.2.1-rc.1", "v1.2.3+build", "1.2"}
	bad := []string{"", "v", "1", "x.y.z", "0.2.x", "control-plane-0.2.1", "latest", "0.2.1.4"}
	for _, v := range good {
		if !looksSemver(v) {
			t.Errorf("looksSemver(%q) = false, want true", v)
		}
	}
	for _, v := range bad {
		if looksSemver(v) {
			t.Errorf("looksSemver(%q) = true, want false", v)
		}
	}
}

func TestCompareSemver(t *testing.T) {
	cases := []struct {
		a, b string
		want int // -1, 0, 1
	}{
		{"0.2.1", "0.2.2", -1},
		{"0.2.2", "0.2.1", 1},
		{"0.2.1", "0.2.1", 0},
		{"0.2.10", "0.2.9", 1},  // numeric, not lexical
		{"0.3.0", "0.2.99", 1},  // minor beats patch
		{"1.0.0", "0.99.99", 1}, // major beats all
		{"v0.2.1", "0.2.1", 0},  // leading v ignored
		{"1.2", "1.2.0", 0},     // missing patch == 0
		{"1.2.0", "1.2", 0},
		{"0.2.1+build", "0.2.1", 0}, // build metadata ignored
		{"0.2.1", "0.3.0-rc.1", -1}, // stable 0.2.1 < 0.3.0 (core wins)
		{"0.3.0", "0.3.0-rc.1", 1},  // stable ranks above its prerelease
		{"0.3.0-rc.1", "0.3.0", -1},
		{"0.3.0-rc.1", "0.3.0-rc.2", -1}, // prerelease compared lexically
	}
	for _, c := range cases {
		got := compareSemver(c.a, c.b)
		if got < 0 {
			got = -1
		} else if got > 0 {
			got = 1
		}
		if got != c.want {
			t.Errorf("compareSemver(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

// TestLatestChartVersionPicksHighestStable guards the core selection invariant
// at the comparator level: given a mixed tag list, the highest stable semver
// after the chart- prefix must be selected (prereleases and non-semver tags
// ignored), mirroring LatestChartVersion's loop.
func TestLatestChartVersionPicksHighestStable(t *testing.T) {
	tags := []string{
		"control-plane-0.2.1",
		"control-plane-0.2.0",
		"control-plane-0.3.0-rc.1", // prerelease: skipped
		"control-plane-0.3.0-rc.2", // prerelease: skipped
		"control-plane-0.2.10",     // highest stable
		"some-other-chart-9.9.9",   // different chart: ignored
		"control-plane-latest",     // not semver: ignored
	}
	const prefix = "control-plane-"
	var best string
	for _, tag := range tags {
		if !strings.HasPrefix(tag, prefix) {
			continue
		}
		ver := strings.TrimPrefix(tag, prefix)
		// mirror LatestChartVersion's prerelease skip (chart-releaser marks a
		// release prerelease when the version carries a - suffix).
		if strings.Contains(ver, "-") {
			continue
		}
		if !looksSemver(ver) {
			continue
		}
		if best == "" || compareSemver(ver, best) > 0 {
			best = ver
		}
	}
	if best != "0.2.10" {
		t.Fatalf("selected %q, want 0.2.10", best)
	}
}
