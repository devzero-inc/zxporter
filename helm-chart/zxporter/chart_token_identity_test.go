package zxporter_test

import (
	"strings"
	"testing"

	"github.com/devzero-inc/zxporter/helm-chart/internal/charttest"
)

var patSpec = charttest.Spec{
	ChartDir: ".",
	BaseArgs: []string{
		"--set", "global.k8sProvider=aws",
		"--set", "zxporter.patToken=dzu-test",
		"--set", "zxporter.kubeContextName=test-cluster",
		"--set", "zxporter.useSecretForToken=true",
	},
}

// A bare data: renders as data: null, and Helm's three-way merge then wipes
// the token zxporter stored in this Secret (KUB-158).
func TestTokenSecretNeverRendersNullData(t *testing.T) {
	for _, obj := range charttest.Objects(t, charttest.Render(t, patSpec)) {
		if obj.Doc["kind"] != "Secret" {
			continue
		}
		if v, ok := obj.Doc["data"]; ok && v == nil {
			t.Errorf("%s renders data: null", charttest.Describe(obj.Doc))
		}
	}
}

func TestClusterIDReachesTheConfig(t *testing.T) {
	if got := envConfig(t, "--set", "zxporter.clusterId=c-7")["CLUSTER_ID"]; got != "c-7" {
		t.Fatalf("CLUSTER_ID=%q", got)
	}
}

func controllerConfigChecksum(t *testing.T, args ...string) string {
	t.Helper()
	for name, template := range charttest.PodTemplates(charttest.Objects(t, charttest.Render(t, patSpec, args...)), true) {
		if !strings.Contains(name, "devzero-zxporter-controller-manager") {
			continue
		}
		return charttest.StringMap(template, "metadata", "annotations")["checksum/config"]
	}
	t.Fatal("no controller-manager pod template rendered")
	return ""
}

// zxporter reads CLUSTER_ID and the rest of its config once at start, so a
// changed ConfigMap must roll the controller (KUB-158).
func TestControllerRollsWhenItsConfigMapChanges(t *testing.T) {
	before := controllerConfigChecksum(t)
	if before == "" {
		t.Fatal("controller pod template has no checksum/config annotation")
	}
	if again := controllerConfigChecksum(t); again != before {
		t.Fatalf("checksum is not deterministic: %q then %q", before, again)
	}
	if after := controllerConfigChecksum(t, "--set", "zxporter.clusterId=c-7"); after == before {
		t.Fatal("changing zxporter.clusterId left checksum/config unchanged")
	}
	hijacked := controllerConfigChecksum(t, "--set-json", `podAnnotations={"checksum/config":"hijacked"}`)
	if hijacked != before {
		t.Fatalf("podAnnotations overrode the chart-owned checksum/config: %q", hijacked)
	}
}

// controllerEnv is the manager container env of the controller-manager pod
// template, by name.
func controllerEnv(t *testing.T, args ...string) map[string]map[string]any {
	t.Helper()
	for name, template := range charttest.PodTemplates(charttest.Objects(t, charttest.Render(t, patSpec, args...)), true) {
		if !strings.Contains(name, "devzero-zxporter-controller-manager") {
			continue
		}
		spec, _ := template["spec"].(map[string]any)
		containers, _ := spec["containers"].([]any)
		for _, c := range containers {
			container, _ := c.(map[string]any)
			if container["name"] != "manager" {
				continue
			}
			out := map[string]map[string]any{}
			env, _ := container["env"].([]any)
			for _, e := range env {
				entry, _ := e.(map[string]any)
				name, _ := entry["name"].(string)
				out[name] = entry
			}
			return out
		}
	}
	t.Fatal("no controller-manager manager container rendered")
	return nil
}

// In the default Secret mode CLUSTER_TOKEN is a pod-start copy of the mirror
// Secret zxporter writes itself, and the chart marks it so, so that a promoted
// standby reads the mirror live. An existingSecret, or ConfigMap mode, is an
// explicit credential and is left unmarked (KUB-158).
func TestClusterTokenSourceMarksOnlyTheMirror(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want any // nil means CLUSTER_TOKEN_SOURCE is absent
	}{
		{name: "default Secret mode", want: "mirror"},
		{name: "existingSecret", args: []string{"--set", "zxporter.existingSecret.name=s"}},
		{name: "ConfigMap mode", args: []string{"--set", "zxporter.useSecretForToken=false"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entry, ok := controllerEnv(t, tc.args...)["CLUSTER_TOKEN_SOURCE"]
			switch {
			case tc.want == nil && ok:
				t.Fatalf("CLUSTER_TOKEN_SOURCE=%v, want it absent", entry["value"])
			case tc.want != nil && (!ok || entry["value"] != tc.want):
				t.Fatalf("CLUSTER_TOKEN_SOURCE=%v, want %v", entry["value"], tc.want)
			}
		})
	}
}
