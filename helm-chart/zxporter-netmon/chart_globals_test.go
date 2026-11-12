package zxporternetmon_test

import (
	"testing"

	"github.com/devzero-inc/zxporter/helm-chart/internal/charttest"
)

// netmonDakrURL returns the DAKR_URL env of the netmon container, and whether
// it is set at all (without it netmon reads zxporter's ConfigMap).
func netmonDakrURL(t *testing.T, args ...string) (string, bool) {
	t.Helper()
	for _, template := range charttest.PodTemplates(charttest.Objects(t, charttest.Render(t, platformSpec, args...)), true) {
		containers, _ := template["spec"].(map[string]any)["containers"].([]any)
		for _, c := range containers {
			env, _ := c.(map[string]any)["env"].([]any)
			for _, e := range env {
				if e := e.(map[string]any); e["name"] == "DAKR_URL" {
					v, _ := e["value"].(string)
					return v, true
				}
			}
		}
	}
	return "", false
}

// global.dakrUrl from the dz-platform umbrella wins over config.dakrUrl; with
// neither set, netmon keeps reading DAKR_URL from zxporter's ConfigMap.
func TestGlobalDakrURL(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
		set  bool
	}{
		{"neither set", nil, "", false},
		{"chart value", charttest.Set("config.dakrUrl=https://own.example"), "https://own.example", true},
		{"global wins", charttest.Set("config.dakrUrl=https://own.example", "global.dakrUrl=https://dakr.example"), "https://dakr.example", true},
		{"global alone", charttest.Set("global.dakrUrl=https://dakr.example"), "https://dakr.example", true},
		{"empty global keeps the chart value", charttest.Set("config.dakrUrl=https://own.example", "global.dakrUrl="), "https://own.example", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, set := netmonDakrURL(t, tc.args...)
			if got != tc.want || set != tc.set {
				t.Errorf("DAKR_URL=%q (set %v), want %q (set %v)", got, set, tc.want, tc.set)
			}
		})
	}
}

// The umbrella's global.imageRegistry and global.imagePullSecrets reach every
// netmon pod.
func TestImageGlobals(t *testing.T) {
	charttest.AssertImageGlobals(t, platformSpec, charttest.ImageGlobals{PullSecretsKey: "imagePullSecrets"})
}

// The repository renders verbatim, as it always did, even a scalar Helm's
// default would treat as empty. The tag is pinned so the release bump, which
// rewrites the default tag, does not break the test.
func TestImageRepositoryScalar(t *testing.T) {
	manifests := charttest.Render(t, platformSpec, charttest.Set("image.repository=0", "image.tag=v9.9.9")...)
	for name, tmpl := range charttest.PodTemplates(charttest.Objects(t, manifests), true) {
		if got := charttest.Images(tmpl); len(got) != 1 || got[0] != "0:v9.9.9" {
			t.Errorf("%s: images %v, want [0:v9.9.9]", name, got)
		}
	}
}
