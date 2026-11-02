package zxporter_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/devzero-inc/zxporter/helm-chart/internal/charttest"
)

// schemaError prefixes every values.schema.json violation, on helm 3 and 4.
const schemaError = "values don't meet the specifications of the schema"

var platformSpec = charttest.Spec{
	ChartDir: ".",
	BaseArgs: []string{
		"--set", "global.k8sProvider=aws",
		"--set", "zxporter.clusterToken=test-token",
		"--set", "zxporter.kubeContextName=test-cluster",
	},
}

// The nodemon subchart's objects are attributed to zxporter-nodemon.
func TestPlatformLabels(t *testing.T) {
	charttest.AssertPlatformLabels(t, platformSpec)
	gcp := charttest.Set("priorityClass.create=true", "global.k8sProvider=gcp")
	charttest.AssertPlatformLabels(t, platformSpec, gcp...)
}

// Releases that already set either platform key through commonLabels keep
// their value: adopting the library adds labels and never rewrites one.
func TestUserCommonLabelsKeepTheirPlatformKeys(t *testing.T) {
	objects := charttest.Objects(t, charttest.Render(t, platformSpec,
		"--set", `commonLabels.app\.kubernetes\.io/part-of=mine`,
		"--set", `global.commonLabels.devzero\.io/component=site`,
	))
	for _, obj := range objects {
		labels := charttest.StringMap(obj.Doc, "metadata", "labels")
		desc := charttest.Describe(obj.Doc)
		if want := "site"; labels[charttest.ComponentLabel] != want {
			t.Errorf("%s: %s = %q, want %q", desc, charttest.ComponentLabel, labels[charttest.ComponentLabel], want)
		}
		want := "mine"
		if obj.FromSubchart() {
			want = charttest.PartOf // a chart-local commonLabels stays in its chart
		}
		if labels[charttest.PartOfLabel] != want {
			t.Errorf("%s: %s = %q, want %q", desc, charttest.PartOfLabel, labels[charttest.PartOfLabel], want)
		}
	}
}

// A null under zxporter-nodemon is not a rejection case here: helm 3 drops
// it and keeps the subchart default, helm 4 deletes the key. The subchart's
// own schema test covers its required keys.
func TestValuesSchema(t *testing.T) {
	rejected := []struct {
		name string
		args []string
		path string
	}{
		{"image removed", []string{"--set", "image=null"}, "image"},
		{"image tag removed", []string{"--set", "image.tag=null"}, "tag"},
		{"cluster context removed", []string{"--set", "zxporter.kubeContextName=null"}, "kubeContextName"},
		{"provider removed", []string{"--set", "global.k8sProvider=null"}, "k8sProvider"},
		{"gpu runtime removed", []string{"--set", "global.gpuRuntime=null"}, "gpuRuntime"},
		{"priority class removed", []string{"--set", "priorityClass=null"}, "priorityClass"},
		{"mpa server removed", []string{"--set", "mpaServer=null"}, "mpaServer"},
		{"high availability removed", []string{"--set", "highAvailability=null"}, "highAvailability"},
		{"zxporter block as a string", []string{"--set", "zxporter=x"}, "zxporter"},
	}
	for _, tc := range rejected {
		t.Run("rejects "+tc.name, func(t *testing.T) {
			_, err := charttest.TryRender(t, platformSpec, tc.args...)
			if err == nil || !strings.Contains(err.Error(), schemaError) ||
				!strings.Contains(err.Error(), tc.path) {
				t.Fatalf("want a schema error naming %q, got %v", tc.path, err)
			}
		})
	}

	accepted := []struct {
		name string
		args []string
	}{
		{"numeric image tag", []string{"--set", "image.tag=123"}},
		{"pdb removed while high availability is off", charttest.Set(
			"highAvailability.enabled=false", "highAvailability.podDisruptionBudget=null")},
		{"cluster token removed with an existing secret", charttest.Set(
			"zxporter.clusterToken=null", "zxporter.existingSecret.name=s")},
		{"runtime class removed in default gpu mode", charttest.Set(
			"global.gpuRuntime.mode=default", "global.gpuRuntime.runtimeClassName=null",
			"zxporter-nodemon.global.gpuRuntime.mode=default")},
		{"unknown key", []string{"--set", "somethingNew=1"}},
	}
	for _, tc := range accepted {
		t.Run("accepts "+tc.name, func(t *testing.T) {
			if _, err := charttest.TryRender(t, platformSpec, tc.args...); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Workload selectors are immutable and the Services route by theirs, so every
// one is pinned. The nodemon DaemonSets' instance label is their fullname, not
// the release name, which keeps them byte-identical to the dist bundles.
func TestSelectorsArePinned(t *testing.T) {
	controller := `{"matchLabels":{"control-plane":"controller-manager"}}`
	want := map[string]string{
		"PodDisruptionBudget/devzero-zxporter-devzero-zxporter-pdb": controller,
		"Service/devzero-zxporter-controller-manager-mpa": `{"app.kubernetes.io/instance":"release",` +
			`"app.kubernetes.io/name":"zxporter"}`,
		"Service/devzero-zxporter-controller-manager-metrics-service": `{"control-plane":"controller-manager"}`,
		"DaemonSet/zxporter-nodemon": `{"matchLabels":{"app.kubernetes.io/instance":"zxporter-nodemon",` +
			`"app.kubernetes.io/name":"zxporter-nodemon"}}`,
		"DaemonSet/zxporter-nodemon-gpu": `{"matchLabels":{"app.kubernetes.io/instance":"zxporter-nodemon-gpu",` +
			`"app.kubernetes.io/name":"zxporter-nodemon"}}`,
		"Deployment/devzero-zxporter-controller-manager": controller,
	}
	got := charttest.Selectors(t, charttest.Objects(t, charttest.Render(t, platformSpec)))
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("selectors changed:\n got %v\nwant %v", got, want)
	}
}
