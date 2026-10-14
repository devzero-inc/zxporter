package zxporternodemon_test

import (
	"reflect"
	"testing"

	"github.com/devzero-inc/zxporter/helm-chart/internal/charttest"
)

// The umbrella's global.imageRegistry and global.imagePullSecrets reach every
// nodemon pod, both DaemonSets and both containers (nodemon and the DCGM
// exporter), in each configuration the dist installers ship.
func TestImageGlobals(t *testing.T) {
	charttest.AssertImageGlobals(t, platformSpec, charttest.ImageGlobals{
		PullSecretsKey: "imagePullSecrets",
		Variants: map[string][]string{
			"low privilege": charttest.Set("runtimeMetrics.enabled=false", "priorityClass.create=false"),
			"without dcgm":  charttest.Set("dcgmExporter.enabled=false"),
		},
	})
}

// Each tag renders as it always did: the DCGM tag verbatim (0 and false
// included), the nodemon tag falling back to the appVersion when empty.
func TestImageTagScalars(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want []string
	}{
		{"dcgm tag 0", charttest.Set("dcgmExporter.image.tag=0"), []string{
			"public.ecr.aws/devzeroinc/zxporter-nodemon:v0.1.21", "public.ecr.aws/devzeroinc/dcgm-exporter:0",
		}},
		{"dcgm tag false", charttest.Set("dcgmExporter.image.tag=false"), []string{
			"public.ecr.aws/devzeroinc/zxporter-nodemon:v0.1.21", "public.ecr.aws/devzeroinc/dcgm-exporter:false",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for name, tmpl := range charttest.PodTemplates(charttest.Objects(t, charttest.Render(t, platformSpec, tc.args...)), true) {
				if got := charttest.Images(tmpl); !reflect.DeepEqual(got, tc.want) {
					t.Errorf("%s: images %v, want %v", name, got, tc.want)
				}
			}
		})
	}
}
