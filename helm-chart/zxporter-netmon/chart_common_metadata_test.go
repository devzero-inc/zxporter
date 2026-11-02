package zxporternetmon_test

import (
	"testing"

	"github.com/devzero-inc/zxporter/helm-chart/internal/charttest"
)

func TestCommonMetadataContract(t *testing.T) {
	charttest.RunContract(t, charttest.Spec{ChartDir: "."})
}
