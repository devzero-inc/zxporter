package zxporternodemon_test

import (
	"encoding/json"
	"testing"

	"github.com/devzero-inc/zxporter/helm-chart/internal/charttest"
)

// Each runtimeMetrics mode grants the nodemon container exactly its privileges:
// runAsRoot (the default) runs as root with SYS_PTRACE under hostPID,
// runAsRoot=false keeps hostPID but uses nodemon.securityContext, and disabled
// has neither.
func TestRuntimeMetricsModes(t *testing.T) {
	const nonRoot = `{"readOnlyRootFilesystem":true,"runAsNonRoot":true}`
	const root = `{"capabilities":{"add":["SYS_PTRACE"]},"readOnlyRootFilesystem":true,"runAsNonRoot":false,` +
		`"runAsUser":0,"seLinuxOptions":{"level":"s0:c0.c1023"}}`
	for _, tc := range []struct {
		name            string
		args            []string
		hostPID         bool
		securityContext string
		runtimeEnv      string
	}{
		{"root by default", charttest.Set("runtimeMetrics.enabled=true"), true, root, "true"},
		{"root explicitly", charttest.Set("runtimeMetrics.enabled=true", "runtimeMetrics.runAsRoot=true"),
			true, root, "true"},
		{"non-root", charttest.Set("runtimeMetrics.enabled=true", "runtimeMetrics.runAsRoot=false"),
			true, nonRoot, "true"},
		{"disabled", charttest.Set("runtimeMetrics.enabled=false"), false, nonRoot, "false"},
		{"runAsRoot ignored when disabled", charttest.Set("runtimeMetrics.enabled=false", "runtimeMetrics.runAsRoot=true"),
			false, nonRoot, "false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			templates := charttest.PodTemplates(charttest.Objects(t, charttest.Render(t, platformSpec, tc.args...)), true)
			if len(templates) == 0 {
				t.Fatal("no pod templates rendered")
			}
			for name, tmpl := range templates {
				spec, _ := tmpl["spec"].(map[string]any)
				if got, _ := spec["hostPID"].(bool); got != tc.hostPID {
					t.Errorf("%s: hostPID = %v, want %v", name, got, tc.hostPID)
				}
				nodemon := container(spec, "zxporter-nodemon")
				if nodemon == nil {
					t.Fatalf("%s: no zxporter-nodemon container", name)
				}
				sc, err := json.Marshal(nodemon["securityContext"])
				if err != nil {
					t.Fatal(err)
				}
				if string(sc) != tc.securityContext {
					t.Errorf("%s: securityContext = %s, want %s", name, sc, tc.securityContext)
				}
				if got := env(nodemon, "RUNTIME_METRICS_ENABLED"); got != tc.runtimeEnv {
					t.Errorf("%s: RUNTIME_METRICS_ENABLED = %q, want %q", name, got, tc.runtimeEnv)
				}
			}
		})
	}
}

func TestRuntimeMetricsRunAsRootMustBeBoolean(t *testing.T) {
	if _, err := charttest.TryRender(t, platformSpec, "--set", "runtimeMetrics.runAsRoot=yes"); err == nil {
		t.Fatal("want a schema error for a non-boolean runtimeMetrics.runAsRoot")
	}
}

func container(spec map[string]any, name string) map[string]any {
	containers, _ := spec["containers"].([]any)
	for _, c := range containers {
		if m, _ := c.(map[string]any); m["name"] == name {
			return m
		}
	}
	return nil
}

func env(container map[string]any, name string) string {
	vars, _ := container["env"].([]any)
	for _, v := range vars {
		if m, _ := v.(map[string]any); m["name"] == name {
			s, _ := m["value"].(string)
			return s
		}
	}
	return ""
}
