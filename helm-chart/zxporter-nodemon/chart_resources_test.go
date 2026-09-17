package zxporternodemon_test

import (
	"bytes"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// resourceKnobs maps every container this chart installs to the values key
// that sets its requests/limits, and to a sentinel CPU request unique to that
// key. A container with no way to override its resources is a deployment
// blocker on any cluster whose namespaces carry a ResourceQuota or a
// LimitRange with no default request: the admission plugin rejects the whole
// pod, so one unsized sidecar takes the entire DaemonSet down with it.
//
// The chart renders the same two containers into both DaemonSet variants (the
// base one and the -gpu one produced by the GPU-node split), so both variants
// are checked.
var resourceKnobs = []struct {
	valuesKey string
	container string
	// cpu is the sentinel request this knob is set to for the render. Each is
	// distinct, so a template that hardcodes resources or reads the wrong key
	// surfaces as a mismatch rather than a silent pass.
	cpu string
}{
	{"nodemon.resources", "zxporter-nodemon", "111m"},
	{"dcgmExporter.resources", "dcgm-exporter", "222m"},
}

type renderedContainer struct {
	workload  string
	container string
	requests  map[string]any
	limits    map[string]any
}

func (c renderedContainer) key() string { return c.workload + "/" + c.container }

func renderChart(t *testing.T, extraArgs ...string) string {
	t.Helper()

	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm binary not available, skipping chart render test")
	}

	chartDir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("resolving chart dir: %v", err)
	}

	args := append([]string{"template", "release", chartDir}, extraArgs...)

	var stdout, stderr bytes.Buffer
	cmd := exec.Command("helm", args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("helm template failed: %v\n%s", err, stderr.String())
	}
	return stdout.String()
}

// collectContainers decodes a rendered manifest and returns every container
// and initContainer of every pod-bearing workload in it, keyed by
// "<workload>/<container>".
func collectContainers(t *testing.T, manifest string) map[string]renderedContainer {
	t.Helper()

	found := map[string]renderedContainer{}
	for doc := range strings.SplitSeq(manifest, "\n---\n") {
		if strings.TrimSpace(doc) == "" {
			continue
		}

		var obj map[string]any
		if err := yaml.Unmarshal([]byte(doc), &obj); err != nil {
			t.Fatalf("unmarshalling rendered manifest: %v\n%s", err, doc)
		}

		spec, _ := obj["spec"].(map[string]any)
		if spec == nil {
			continue
		}
		template, _ := spec["template"].(map[string]any)
		podSpec, _ := template["spec"].(map[string]any)
		if podSpec == nil {
			continue
		}

		meta, _ := obj["metadata"].(map[string]any)
		name, _ := meta["name"].(string)

		for _, field := range []string{"initContainers", "containers"} {
			list, _ := podSpec[field].([]any)
			for _, entry := range list {
				container, _ := entry.(map[string]any)
				containerName, _ := container["name"].(string)
				resources, _ := container["resources"].(map[string]any)
				requests, _ := resources["requests"].(map[string]any)
				limits, _ := resources["limits"].(map[string]any)

				rc := renderedContainer{
					workload:  name,
					container: containerName,
					requests:  requests,
					limits:    limits,
				}
				found[rc.key()] = rc
			}
		}
	}
	return found
}

// TestEveryInstalledContainerHasAnOverridableResourcesKnob is the guard that
// keeps requests/limits overridable for everything this chart installs. It
// asserts both directions:
//
//   - every knob in resourceKnobs reaches its container (no template may
//     hardcode resources or read the wrong values key), and
//   - every container the chart renders appears in resourceKnobs (adding a
//     container without a values key fails here, rather than in a customer's
//     quota-constrained namespace).
func TestEveryInstalledContainerHasAnOverridableResourcesKnob(t *testing.T) {
	args := []string{
		"--namespace", "devzero-system",
		"--set", "global.k8sProvider=aws",
	}
	for _, knob := range resourceKnobs {
		args = append(args, "--set-json", fmt.Sprintf(
			`%s={"requests":{"cpu":%q,"memory":"64Mi"},"limits":{"cpu":%q,"memory":"128Mi"}}`,
			knob.valuesKey, knob.cpu, knob.cpu,
		))
	}

	rendered := collectContainers(t, renderChart(t, args...))
	if len(rendered) == 0 {
		t.Fatal("chart rendered no containers; the render arguments are stale")
	}

	byContainer := map[string]string{}
	for _, knob := range resourceKnobs {
		byContainer[knob.container] = knob.cpu
	}

	seen := map[string]bool{}
	var unknown []string
	for key, container := range rendered {
		wantCPU, ok := byContainer[container.container]
		if !ok {
			unknown = append(unknown, key)
			continue
		}
		seen[container.container] = true

		if got := container.requests["cpu"]; got != wantCPU {
			t.Errorf("%s: cpu request is %v, want %q -- the template is not reading its values key",
				key, got, wantCPU)
		}
		if got := container.limits["cpu"]; got != wantCPU {
			t.Errorf("%s: cpu limit is %v, want %q -- the template drops limits",
				key, got, wantCPU)
		}
	}

	for _, knob := range resourceKnobs {
		if !seen[knob.container] {
			t.Errorf("container %q did not render; resourceKnobs is stale", knob.container)
		}
	}

	sort.Strings(unknown)
	if len(unknown) > 0 {
		t.Errorf("containers render with no entry in resourceKnobs: %v\n"+
			"Give each one a values key for its resources and add it to the table. "+
			"A container with hardcoded resources cannot be sized by an operator, "+
			"and one with none at all is rejected outright by a namespace "+
			"ResourceQuota that requires requests.", unknown)
	}
}

// TestChartDefaultsGiveEveryContainerResourceRequests guards the default
// render: a fresh install with no overrides must still request CPU and memory
// for every container, so `helm install` succeeds in a namespace whose
// ResourceQuota requires requests. Both the split (GPU-node) and non-split
// layouts are covered, on a provider that takes the host-driver path and one
// that does not.
func TestChartDefaultsGiveEveryContainerResourceRequests(t *testing.T) {
	for _, args := range [][]string{
		// Split layout: base DaemonSet plus the -gpu one.
		{"--namespace", "devzero-system", "--set", "global.k8sProvider=aws"},
		// Non-split layout, host-mounted driver path.
		{
			"--namespace", "devzero-system",
			"--set", "global.k8sProvider=gcp",
			"--set", "dcgmExporter.runtimeClassName=",
		},
	} {
		rendered := collectContainers(t, renderChart(t, args...))
		if len(rendered) == 0 {
			t.Fatalf("chart rendered no containers for %v", args)
		}
		for key, container := range rendered {
			if container.requests["cpu"] == nil {
				t.Errorf("%s has no default cpu request (rendered with %v)", key, args)
			}
			if container.requests["memory"] == nil {
				t.Errorf("%s has no default memory request (rendered with %v)", key, args)
			}
		}
	}
}
