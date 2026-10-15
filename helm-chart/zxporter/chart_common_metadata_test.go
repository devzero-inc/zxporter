package zxporter_test

import (
	"testing"

	"github.com/devzero-inc/zxporter/helm-chart/internal/charttest"
)

// The zxporter chart pulls zxporter-nodemon in as a local subchart, so this
// render covers both. A chart-local commonLabels reaches only the chart it is
// set on; global.commonLabels is what covers the whole release, which the
// contract asserts separately.
func TestCommonMetadataContract(t *testing.T) {
	charttest.RunContract(t, charttest.Spec{
		ChartDir: ".",
		BaseArgs: []string{
			"--set", "global.k8sProvider=aws",
			"--set", "zxporter.clusterToken=test-token",
			"--set", "zxporter.kubeContextName=test-cluster",
		},
		SubchartPrefix: "zxporter/charts/zxporter-nodemon/",
	})
}
