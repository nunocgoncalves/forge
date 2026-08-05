package e2e

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/nunocgoncalves/forge/test/e2e/internal/kindtest"
)

// installPlatformCertificateSubstrate mirrors Forge's 0.3+ release boundary for
// Kind scenarios that install the platform chart directly. Older platform
// charts keep their bundled cert-manager behavior.
func installPlatformCertificateSubstrate(
	t *testing.T,
	cluster *kindtest.Cluster,
	platformRelease, platformRef, platformVersion, namespace, localPlatform string,
) {
	t.Helper()
	effectiveVersion := platformVersion
	if effectiveVersion == "" && localPlatform != "" {
		effectiveVersion = kindtest.ChartAppVersion(t, platformRef, platformVersion, localPlatform)
	}
	if !kindtest.ChartVersionAtLeast(effectiveVersion, "0.3.0") {
		return
	}

	substrateRef := strings.TrimSuffix(platformRef, "/iterabase-platform") + "/cert-manager-substrate"
	if substrateRef == platformRef {
		t.Fatalf("platform chart reference %q must end in /iterabase-platform", platformRef)
	}
	localSubstrate := ""
	if localPlatform != "" {
		localSubstrate = filepath.Join(filepath.Dir(localPlatform), "cert-manager-substrate")
	}
	cluster.HelmInstall(t, platformRelease+"-cert-manager", substrateRef, effectiveVersion, namespace, localSubstrate, map[string]string{
		"cert-manager.prometheus.servicemonitor.enabled": "false",
	})
}
