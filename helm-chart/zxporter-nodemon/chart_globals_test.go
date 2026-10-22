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

// The DCGM tag renders verbatim, as it always did, 0 and false included. The
// nodemon tag is pinned so the release bump, which rewrites the default tag,
// does not break the test.
func TestImageTagScalars(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want []string
	}{
		{"dcgm tag 0", charttest.Set("image.tag=v9.9.9", "dcgmExporter.image.tag=0"), []string{
			"public.ecr.aws/devzeroinc/zxporter-nodemon:v9.9.9", "public.ecr.aws/devzeroinc/dcgm-exporter:0",
		}},
		{"dcgm tag false", charttest.Set("image.tag=v9.9.9", "dcgmExporter.image.tag=false"), []string{
			"public.ecr.aws/devzeroinc/zxporter-nodemon:v9.9.9", "public.ecr.aws/devzeroinc/dcgm-exporter:false",
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
