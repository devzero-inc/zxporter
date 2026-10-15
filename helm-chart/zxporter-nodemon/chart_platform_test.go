package zxporternodemon_test

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
	BaseArgs: []string{"--set", "global.k8sProvider=aws"},
}

func TestPlatformLabels(t *testing.T) {
	charttest.AssertPlatformLabels(t, platformSpec)
	// The low-privilege bundle the dist installers ship.
	lowPriv := charttest.Set("runtimeMetrics.enabled=false", "priorityClass.create=false")
	charttest.AssertPlatformLabels(t, platformSpec, lowPriv...)
}

func TestUserCommonLabelsKeepTheirPlatformKeys(t *testing.T) {
	objects := charttest.Objects(t, charttest.Render(t, platformSpec,
		"--set", `commonLabels.app\.kubernetes\.io/part-of=mine`,
	))
	for _, obj := range objects {
		if got := charttest.StringMap(obj.Doc, "metadata", "labels")[charttest.PartOfLabel]; got != "mine" {
			t.Errorf("%s: %s = %q, want %q", charttest.Describe(obj.Doc), charttest.PartOfLabel, got, "mine")
		}
	}
}

func TestValuesSchema(t *testing.T) {
	rejected := []struct {
		name string
		args []string
		path string
	}{
		{"image removed", []string{"--set", "image=null"}, "image"},
		{"nodemon removed", []string{"--set", "nodemon=null"}, "nodemon"},
		{"rbac removed", []string{"--set", "nodemon.rbac=null"}, "rbac"},
		{"jvm metrics removed", []string{"--set", "jvmMetrics=null"}, "jvmMetrics"},
		{"provider removed", []string{"--set", "global.k8sProvider=null"}, "k8sProvider"},
		{"dcgm exporter removed", []string{"--set", "dcgmExporter=null"}, "dcgmExporter"},
		{"gpu node selector removed", []string{"--set", "dcgmExporter.gpuNodeSelector=null"}, "gpuNodeSelector"},
		{"service account as a string", []string{"--set", "serviceAccount=x"}, "serviceAccount"},
		{"extra env as a string", []string{"--set", "dcgmExporter.extraEnv=x"}, "extraEnv"},
	}
	for _, tc := range rejected {
		t.Run("rejects "+tc.name, func(t *testing.T) {
			_, err := charttest.TryRender(t, platformSpec, tc.args...)
			if err == nil || !strings.Contains(err.Error(), schemaError) || !strings.Contains(err.Error(), tc.path) {
				t.Fatalf("want a schema error naming %q, got %v", tc.path, err)
			}
		})
	}

	accepted := []struct {
		name string
		args []string
	}{
		{"numeric image tag", []string{"--set", "image.tag=123"}},
		{"dcgm image removed while dcgm is off", charttest.Set("dcgmExporter.enabled=false", "dcgmExporter.image=null")},
		{"runtime class removed in default gpu mode", charttest.Set(
			"global.gpuRuntime.mode=default", "global.gpuRuntime.runtimeClassName=null")},
		{"tolerations as a string, which rendered before the schema", []string{"--set", "nodemon.tolerations=x"}},
	}
	for _, tc := range accepted {
		t.Run("accepts "+tc.name, func(t *testing.T) {
			if _, err := charttest.TryRender(t, platformSpec, tc.args...); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// DaemonSet selectors are immutable; the instance label is the fullname, not
// the release name, which keeps any release byte-identical to the dist bundles.
func TestSelectorsArePinned(t *testing.T) {
	want := map[string]string{
		"DaemonSet/zxporter-nodemon": `{"matchLabels":{"app.kubernetes.io/instance":"zxporter-nodemon",` +
			`"app.kubernetes.io/name":"zxporter-nodemon"}}`,
		"DaemonSet/zxporter-nodemon-gpu": `{"matchLabels":{"app.kubernetes.io/instance":"zxporter-nodemon-gpu",` +
			`"app.kubernetes.io/name":"zxporter-nodemon"}}`,
	}
	got := charttest.Selectors(t, charttest.Objects(t, charttest.Render(t, platformSpec)))
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("selectors changed:\n got %v\nwant %v", got, want)
	}
}
