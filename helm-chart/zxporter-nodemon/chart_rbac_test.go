package zxporternodemon_test

import (
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// TestPodsRuleGrantsListAndWatchForEveryFeatureCombination guards the RBAC the
// node-scoped pod informer (PodContainerIndex) needs. nodemon starts that
// informer when cgroupMetrics OR runtimeMetrics is enabled, and an informer
// needs both list and watch. Gating watch on runtimeMetrics alone left every
// runtimeMetrics.enabled=false install (including the -lowpriv dist bundles)
// failing with "cannot watch resource pods" and silently dropping all cgroup
// signals.
func TestPodsRuleGrantsListAndWatchForEveryFeatureCombination(t *testing.T) {
	for _, tc := range []struct {
		runtime, cgroup string
	}{
		{"true", "true"},
		{"false", "true"},
		{"true", "false"},
		{"false", "false"},
	} {
		t.Run("runtime="+tc.runtime+",cgroup="+tc.cgroup, func(t *testing.T) {
			manifest := renderChart(t,
				"--set", "global.k8sProvider=other",
				"--set", "runtimeMetrics.enabled="+tc.runtime,
				"--set", "cgroupMetrics.enabled="+tc.cgroup,
			)

			verbs := podsRuleVerbs(t, manifest)
			for _, want := range []string{"get", "list", "watch"} {
				if !slices.Contains(verbs, want) {
					t.Errorf("pods rule verbs = %v, missing %q", verbs, want)
				}
			}
		})
	}
}

// podsRuleVerbs returns the verbs of the core-group pods rule in the chart's
// ClusterRole or Role, failing the test if there is not exactly one.
func podsRuleVerbs(t *testing.T, manifest string) []string {
	t.Helper()

	var found [][]string
	for doc := range strings.SplitSeq(manifest, "\n---\n") {
		var obj struct {
			Kind  string `json:"kind"`
			Rules []struct {
				APIGroups []string `json:"apiGroups"`
				Resources []string `json:"resources"`
				Verbs     []string `json:"verbs"`
			} `json:"rules"`
		}
		if err := yaml.Unmarshal([]byte(doc), &obj); err != nil {
			t.Fatalf("unmarshalling rendered manifest: %v\n%s", err, doc)
		}
		if obj.Kind != "ClusterRole" && obj.Kind != "Role" {
			continue
		}
		for _, r := range obj.Rules {
			if slices.Contains(r.APIGroups, "") && slices.Contains(r.Resources, "pods") {
				found = append(found, r.Verbs)
			}
		}
	}
	if len(found) != 1 {
		t.Fatalf("expected exactly one core-group pods rule, found %d", len(found))
	}
	return found[0]
}
