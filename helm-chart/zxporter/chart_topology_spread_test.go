package zxporter_test

import (
	"reflect"
	"testing"

	"github.com/devzero-inc/zxporter/helm-chart/internal/charttest"
)

const (
	zoneSpread     = `{"maxSkew":1,"topologyKey":"topology.kubernetes.io/zone","whenUnsatisfiable":"ScheduleAnyway"}`
	hostnameSpread = `{"maxSkew":1,"topologyKey":"kubernetes.io/hostname","whenUnsatisfiable":"DoNotSchedule","labelSelector":{"matchLabels":{"custom":"selector"}}}`
)

var topologySpreadSpec = charttest.Spec{
	ChartDir: ".",
	BaseArgs: []string{
		"--set", "global.k8sProvider=aws",
		"--set", "zxporter.clusterToken=test-token",
		"--set", "zxporter.kubeContextName=test-cluster",
	},
}

func TestTopologySpreadConstraints(t *testing.T) {
	tests := []struct {
		name string
		args []string
		// want maps each constraint's topologyKey to the labelSelector.matchLabels
		// it must carry; nil means the pod's own labels.
		want map[string]map[string]string
	}{
		{name: "unset renders none"},
		{
			name: "entry without labelSelector selects the controller-manager pods",
			args: charttest.SetJSON("topologySpreadConstraints", "["+zoneSpread+"]"),
			want: map[string]map[string]string{"topology.kubernetes.io/zone": nil},
		},
		{
			name: "explicit labelSelector is kept",
			args: charttest.SetJSON("topologySpreadConstraints", "["+zoneSpread+","+hostnameSpread+"]"),
			want: map[string]map[string]string{
				"topology.kubernetes.io/zone": nil,
				"kubernetes.io/hostname":      {"custom": "selector"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manifests := charttest.Render(t, topologySpreadSpec, tt.args...)
			if dups := charttest.DuplicateKeys(t, manifests); len(dups) > 0 {
				t.Fatalf("duplicate keys: %v", dups)
			}

			sawDeployment := false
			for name, template := range charttest.PodTemplates(charttest.Objects(t, manifests), false) {
				constraints, _ := template["spec"].(map[string]any)["topologySpreadConstraints"].([]any)
				if name != "Deployment/devzero-zxporter-controller-manager" {
					if len(constraints) > 0 {
						t.Errorf("%s: got topologySpreadConstraints %v, want none", name, constraints)
					}
					continue
				}
				sawDeployment = true

				podLabels := charttest.StringMap(template, "metadata", "labels")
				got := map[string]map[string]string{}
				for _, c := range constraints {
					constraint, _ := c.(map[string]any)
					key, _ := constraint["topologyKey"].(string)
					got[key] = charttest.StringMap(constraint, "labelSelector", "matchLabels")
				}
				if len(got) != len(tt.want) {
					t.Fatalf("%s: got constraints %v, want keys %v", name, got, tt.want)
				}
				for key, wantSelector := range tt.want {
					gotSelector, ok := got[key]
					if !ok {
						t.Errorf("%s: missing constraint on %q", name, key)
						continue
					}
					if wantSelector != nil {
						if !reflect.DeepEqual(gotSelector, wantSelector) {
							t.Errorf("%s: %q selects %v, want %v", name, key, gotSelector, wantSelector)
						}
						continue
					}
					if len(gotSelector) == 0 {
						t.Errorf("%s: %q has no default labelSelector", name, key)
					}
					for k, v := range gotSelector {
						if podLabels[k] != v {
							t.Errorf("%s: %q selects %s=%s, which the pod does not carry", name, key, k, v)
						}
					}
				}
			}
			if !sawDeployment {
				t.Fatal("controller-manager Deployment not rendered")
			}
		})
	}
}
