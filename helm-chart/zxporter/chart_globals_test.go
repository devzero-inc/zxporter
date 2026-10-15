package zxporter_test

import (
	"testing"

	"github.com/devzero-inc/zxporter/helm-chart/internal/charttest"
)

// envConfig returns the data of the ConfigMap that carries KUBE_CONTEXT_NAME.
func envConfig(t *testing.T, args ...string) map[string]string {
	t.Helper()
	for _, obj := range charttest.Objects(t, charttest.Render(t, platformSpec, args...)) {
		if obj.Doc["kind"] != "ConfigMap" {
			continue
		}
		if data := charttest.StringMap(obj.Doc, "data"); data["KUBE_CONTEXT_NAME"] != "" {
			return data
		}
	}
	t.Fatal("no ConfigMap carries KUBE_CONTEXT_NAME")
	return nil
}

// The dz-platform umbrella sets the cluster's identity once, under global.*,
// and it wins over zxporter's own keys (whose dakrUrl defaults to production,
// so a chart-first rule would ignore the umbrella). A release that sets no
// global, or an empty one, renders exactly as before.
func TestGlobalClusterIdentity(t *testing.T) {
	for _, tc := range []struct {
		name          string
		args          []string
		context, dakr string
	}{
		{"chart values without globals", nil, "test-cluster", "https://dakr.devzero.io"},
		{"globals win", charttest.Set("global.clusterName=umbrella", "global.dakrUrl=https://dakr.example"), "umbrella", "https://dakr.example"},
		{"empty globals keep the chart values", charttest.Set("global.clusterName=", "global.dakrUrl="), "test-cluster", "https://dakr.devzero.io"},
		{"global.clusterName stands in for kubeContextName", charttest.Set("zxporter.kubeContextName=", "global.clusterName=umbrella"), "umbrella", "https://dakr.devzero.io"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := envConfig(t, tc.args...)
			if data["KUBE_CONTEXT_NAME"] != tc.context || data["DAKR_URL"] != tc.dakr {
				t.Errorf("KUBE_CONTEXT_NAME=%q DAKR_URL=%q, want %q %q", data["KUBE_CONTEXT_NAME"], data["DAKR_URL"], tc.context, tc.dakr)
			}
		})
	}

	_, err := charttest.TryRender(t, platformSpec, charttest.Set("zxporter.kubeContextName=", "global.clusterName=")...)
	if err == nil {
		t.Error("rendered with neither zxporter.kubeContextName nor global.clusterName")
	}
}

// A tag the chart always printed verbatim keeps rendering verbatim, including
// the scalars Helm's default would treat as empty.
func TestImageTagScalars(t *testing.T) {
	for _, tc := range []struct{ tag, want string }{
		{"0", "public.ecr.aws/devzeroinc/zxporter:0"},
		{"false", "public.ecr.aws/devzeroinc/zxporter:false"},
	} {
		for name, templates := range charttest.PodTemplates(charttest.Objects(t, charttest.Render(t, platformSpec, "--set", "image.tag="+tc.tag)), true) {
			if got := charttest.Images(templates); len(got) != 1 || got[0] != tc.want {
				t.Errorf("%s with image.tag=%s: images %v, want [%s]", name, tc.tag, got, tc.want)
			}
		}
	}
}

// The umbrella's global.imageRegistry and global.imagePullSecrets reach every
// pod this chart renders. The bundled nodemon is covered in its own package.
func TestImageGlobals(t *testing.T) {
	charttest.AssertImageGlobals(t, platformSpec, charttest.ImageGlobals{
		PullSecretsKey: "imagePullSecrets",
		OwnChartOnly:   true,
	})
}
