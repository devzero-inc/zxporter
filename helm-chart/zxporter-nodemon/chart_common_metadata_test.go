package zxporternodemon_test

import (
	"testing"

	"github.com/devzero-inc/zxporter/helm-chart/internal/charttest"
)

// zxporter-nodemon is published standalone as well as being a subchart of
// zxporter, so it carries the knobs in its own right rather than only inheriting
// global values from the parent.
func TestCommonMetadataContract(t *testing.T) {
	charttest.RunContract(t, charttest.Spec{
		ChartDir: ".",
		BaseArgs: []string{"--set", "global.k8sProvider=aws"},
		Variants: map[string][]string{
			// The DaemonSet's annotations used to be emitted only under
			// `if $v.withDcgm`, so a non-GPU install silently dropped
			// commonAnnotations while the default render stayed green.
			"dcgm disabled": {"--set", "dcgmExporter.enabled=false"},
		},
	})
}
