package zxporternetmon_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/devzero-inc/zxporter/helm-chart/internal/charttest"
)

// schemaError prefixes every values.schema.json violation, on helm 3 and 4.
const schemaError = "values don't meet the specifications of the schema"

var platformSpec = charttest.Spec{ChartDir: "."}

func TestPlatformLabels(t *testing.T) {
	charttest.AssertPlatformLabels(t, platformSpec)
	charttest.AssertPlatformLabels(t, platformSpec, "--set", "serviceAccount.create=false")
}

func TestUserCommonLabelsKeepTheirPlatformKeys(t *testing.T) {
	objects := charttest.Objects(t, charttest.Render(t, platformSpec,
		"--set", `commonLabels.devzero\.io/component=custom`,
	))
	for _, obj := range objects {
		if got := charttest.StringMap(obj.Doc, "metadata", "labels")[charttest.ComponentLabel]; got != "custom" {
			t.Errorf("%s: %s = %q, want %q", charttest.Describe(obj.Doc), charttest.ComponentLabel, got, "custom")
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
		{"service account removed", []string{"--set", "serviceAccount=null"}, "serviceAccount"},
		{"config removed", []string{"--set", "config=null"}, "config"},
		{"config as a string", []string{"--set", "config=x"}, "config"},
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
		{"tolerations as a string, which rendered before the schema", []string{"--set", "tolerations=x"}},
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

// The DaemonSet selector is immutable, so it is pinned exactly.
func TestSelectorsArePinned(t *testing.T) {
	want := map[string]string{
		"DaemonSet/release-zxporter-netmon": `{"matchLabels":{"app":"zxporter-netmon",` +
			`"app.kubernetes.io/instance":"release","app.kubernetes.io/name":"zxporter-netmon"}}`,
	}
	got := charttest.Selectors(t, charttest.Objects(t, charttest.Render(t, platformSpec)))
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("selectors changed:\n got %v\nwant %v", got, want)
	}
}
