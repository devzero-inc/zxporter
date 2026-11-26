package zxporternodemon_test

import (
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/devzero-inc/zxporter/helm-chart/internal/charttest"
)

const (
	nodemonImage = "public.ecr.aws/devzeroinc/zxporter-nodemon"
	dcgmImage    = "public.ecr.aws/devzeroinc/dcgm-exporter"
)

// placement is what one node gets: the single DaemonSet that schedules there,
// the images its pod pulls, and the RuntimeClass it asks for.
type placement struct {
	daemonSet    string
	images       []string
	runtimeClass string
}

// renderedDaemonSet is the part of a rendered DaemonSet that decides placement.
type renderedDaemonSet struct {
	name         string
	terms        []any // nil: no required node affinity, schedules everywhere
	images       []string
	runtimeClass string
}

func daemonSets(t *testing.T, args ...string) []renderedDaemonSet {
	t.Helper()
	objects := charttest.Objects(t, charttest.Render(t, platformSpec, args...))
	out := make([]renderedDaemonSet, 0, len(objects))
	for _, obj := range objects {
		if obj.Doc["kind"] != "DaemonSet" {
			continue
		}
		meta, _ := obj.Doc["metadata"].(map[string]any)
		spec, _ := obj.Doc["spec"].(map[string]any)
		tmpl, _ := spec["template"].(map[string]any)
		podSpec, _ := tmpl["spec"].(map[string]any)

		ds := renderedDaemonSet{name: meta["name"].(string)}
		ds.runtimeClass, _ = podSpec["runtimeClassName"].(string)
		containers, _ := podSpec["containers"].([]any)
		for _, c := range containers {
			image, _ := c.(map[string]any)["image"].(string)
			repo, _, _ := strings.Cut(image, ":")
			ds.images = append(ds.images, repo)
		}
		affinity, _ := podSpec["affinity"].(map[string]any)
		nodeAffinity, _ := affinity["nodeAffinity"].(map[string]any)
		required, _ := nodeAffinity["requiredDuringSchedulingIgnoredDuringExecution"].(map[string]any)
		if required != nil {
			ds.terms, _ = required["nodeSelectorTerms"].([]any)
			if ds.terms == nil {
				ds.terms = []any{}
			}
		}
		out = append(out, ds)
	}
	return out
}

// matches evaluates requiredDuringSchedulingIgnoredDuringExecution the way the
// scheduler does: the terms are OR'd, a term's expressions are AND'd, NotIn
// and DoesNotExist match a node without the label, and Gt/Lt need the label
// and compare it as an integer (a non-integer value matches neither).
func (ds renderedDaemonSet) matches(t *testing.T, labels map[string]string) bool {
	t.Helper()
	if ds.terms == nil {
		return true
	}
	for _, raw := range ds.terms {
		term, _ := raw.(map[string]any)
		exprs, _ := term["matchExpressions"].([]any)
		all := true
		for _, rawExpr := range exprs {
			if !exprMatches(t, rawExpr.(map[string]any), labels) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

func exprMatches(t *testing.T, expr map[string]any, labels map[string]string) bool {
	t.Helper()
	key, _ := expr["key"].(string)
	op, _ := expr["operator"].(string)
	raw, _ := expr["values"].([]any)
	values := make([]string, 0, len(raw))
	for _, v := range raw {
		values = append(values, v.(string))
	}
	value, present := labels[key]
	in := func() bool {
		for _, v := range values {
			if v == value {
				return true
			}
		}
		return false
	}
	switch op {
	case "In":
		return present && in()
	case "NotIn":
		return !present || !in()
	case "Exists":
		return present
	case "DoesNotExist":
		return !present
	case "Gt", "Lt":
		if !present || len(values) != 1 {
			return false
		}
		got, err := strconv.Atoi(value)
		if err != nil {
			return false
		}
		bound, err := strconv.Atoi(values[0])
		if err != nil {
			t.Fatalf("%s %s: non-integer bound %q", key, op, values[0])
		}
		if op == "Gt" {
			return got > bound
		}
		return got < bound
	}
	t.Fatalf("unknown operator %q", op)
	return false
}

// place returns the one DaemonSet that schedules on a node with these labels,
// failing when none or more than one does: two would run two nodemons on the
// node, none would leave it without telemetry.
func place(t *testing.T, sets []renderedDaemonSet, labels map[string]string) placement {
	t.Helper()
	var hits []renderedDaemonSet
	for _, ds := range sets {
		if ds.matches(t, labels) {
			hits = append(hits, ds)
		}
	}
	if len(hits) != 1 {
		names := make([]string, 0, len(hits))
		for _, h := range hits {
			names = append(names, h.name)
		}
		t.Fatalf("node %v: want exactly one DaemonSet, got %v", labels, names)
	}
	return placement{daemonSet: hits[0].name, images: hits[0].images, runtimeClass: hits[0].runtimeClass}
}

var (
	cpuOnly  = []string{nodemonImage}
	withDCGM = []string{nodemonImage, dcgmImage}
)

// Every node class lands on exactly one DaemonSet, and only GPU nodes pull the
// dcgm-exporter image. The default render (no dcgmExporter overrides) is the
// first case: that is what every customer install gets.
func TestDCGMExporterRunsOnlyOnGPUNodes(t *testing.T) {
	nodes := map[string]map[string]string{
		"cpu node, no labels":       {},
		"karpenter cpu node":        {"karpenter.k8s.aws/instance-family": "m7i"},
		"non-integer gpu count":     {"karpenter.k8s.aws/instance-gpu-count": "unknown"},
		"gpu operator node":         {"nvidia.com/gpu.present": "true"},
		"gpu operator + eks labels": {"nvidia.com/gpu.present": "true", "k8s.amazonaws.com/accelerator": "nvidia-tesla-t4"},
		"nfd nvidia pci":            {"feature.node.kubernetes.io/pci-10de.present": "true"},
		"eks gpu ami":               {"k8s.amazonaws.com/accelerator": "nvidia-tesla-t4"},
		"karpenter gpu node": {
			"karpenter.k8s.aws/instance-gpu-manufacturer": "nvidia",
			"karpenter.k8s.aws/instance-gpu-count":        "4",
		},
		"gke gpu pool": {"cloud.google.com/gke-accelerator": "nvidia-l4"},
		"aks gpu pool": {"kubernetes.azure.com/accelerator": "nvidia"},
	}
	base := placement{"zxporter-nodemon", cpuOnly, ""}
	operator := placement{"zxporter-nodemon-gpu", withDCGM, "nvidia"}
	plain := placement{"zxporter-nodemon-gpu-default-runtime", withDCGM, ""}

	for _, tc := range []struct {
		name string
		args []string
		want map[string]placement
	}{
		{
			name: "default values",
			want: map[string]placement{
				"cpu node, no labels":       base,
				"karpenter cpu node":        base,
				"non-integer gpu count":     base,
				"gpu operator node":         operator,
				"gpu operator + eks labels": operator,
				"nfd nvidia pci":            plain,
				"eks gpu ami":               plain,
				"karpenter gpu node":        plain,
				"gke gpu pool":              plain,
				"aks gpu pool":              plain,
			},
		},
		{
			// runtimeClassName="" (GKE COS host-mounted driver): no RuntimeClass
			// DaemonSet, so GPU Operator nodes join the default-runtime one.
			name: "no runtime class",
			args: charttest.Set("dcgmExporter.runtimeClassName=", "global.k8sProvider=gcp"),
			want: map[string]placement{
				"cpu node, no labels":       base,
				"karpenter cpu node":        base,
				"non-integer gpu count":     base,
				"gpu operator node":         plain,
				"gpu operator + eks labels": plain,
				"nfd nvidia pci":            plain,
				"eks gpu ami":               plain,
				"karpenter gpu node":        plain,
				"gke gpu pool":              plain,
				"aks gpu pool":              plain,
			},
		},
		{
			// The layout before gpuOnly: the sidecar idles on every non-GPU-Operator node.
			name: "everyNode",
			args: charttest.Set("dcgmExporter.mode=everyNode"),
			want: map[string]placement{
				"cpu node, no labels":       {"zxporter-nodemon", withDCGM, ""},
				"karpenter cpu node":        {"zxporter-nodemon", withDCGM, ""},
				"non-integer gpu count":     {"zxporter-nodemon", withDCGM, ""},
				"gpu operator node":         operator,
				"gpu operator + eks labels": operator,
				"nfd nvidia pci":            {"zxporter-nodemon", withDCGM, ""},
				"eks gpu ami":               {"zxporter-nodemon", withDCGM, ""},
				"karpenter gpu node":        {"zxporter-nodemon", withDCGM, ""},
				"gke gpu pool":              {"zxporter-nodemon", withDCGM, ""},
				"aks gpu pool":              {"zxporter-nodemon", withDCGM, ""},
			},
		},
		{
			name: "dcgm disabled",
			args: charttest.Set("dcgmExporter.enabled=false"),
			want: map[string]placement{
				"cpu node, no labels":       base,
				"karpenter cpu node":        base,
				"non-integer gpu count":     base,
				"gpu operator node":         base,
				"gpu operator + eks labels": base,
				"nfd nvidia pci":            base,
				"eks gpu ami":               base,
				"karpenter gpu node":        base,
				"gke gpu pool":              base,
				"aks gpu pool":              base,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sets := daemonSets(t, tc.args...)
			for node, labels := range nodes {
				got := place(t, sets, labels)
				want := tc.want[node]
				if got.daemonSet != want.daemonSet || got.runtimeClass != want.runtimeClass ||
					strings.Join(got.images, ",") != strings.Join(want.images, ",") {
					t.Errorf("%s: got %+v, want %+v", node, got, want)
				}
			}
		})
	}
}

// A customer label added to gpuNodeSelectors, an overridden GPU Operator
// selector, and a nodemon.affinity of their own all keep every node on exactly
// one DaemonSet, and the user's affinity still restricts all of them.
func TestGPUPlacementHonoursOverrides(t *testing.T) {
	sets := daemonSets(t,
		"--set", "dcgmExporter.gpuNodeSelector.key=example.com/gpu-operator",
		"--set", "dcgmExporter.gpuNodeSelector.value=yes",
		"--set-json", `dcgmExporter.gpuNodeSelectors=[{"key":"example.com/gpu","operator":"In","values":["a100","h100"]},`+
			`{"key":"example.com/accelerator","operator":"Exists"}]`,
		"--set-json", `nodemon.affinity={"nodeAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":`+
			`{"nodeSelectorTerms":[`+
			`{"matchExpressions":[{"key":"pool","operator":"In","values":["a"]}]},`+
			`{"matchExpressions":[{"key":"pool","operator":"In","values":["b"]}]}]}}}`,
	)
	for _, tc := range []struct {
		labels map[string]string
		want   string
	}{
		{map[string]string{"pool": "a"}, "zxporter-nodemon"},
		{map[string]string{"pool": "b", "example.com/gpu": "t4"}, "zxporter-nodemon"},
		{map[string]string{"pool": "a", "example.com/gpu-operator": "yes"}, "zxporter-nodemon-gpu"},
		{map[string]string{"pool": "b", "example.com/gpu": "h100"}, "zxporter-nodemon-gpu-default-runtime"},
		{map[string]string{"pool": "a", "example.com/accelerator": "x"}, "zxporter-nodemon-gpu-default-runtime"},
		// The defaults were replaced, so this label no longer marks a GPU node.
		{map[string]string{"pool": "a", "k8s.amazonaws.com/accelerator": "x"}, "zxporter-nodemon"},
	} {
		if got := place(t, sets, tc.labels).daemonSet; got != tc.want {
			t.Errorf("node %v: got %s, want %s", tc.labels, got, tc.want)
		}
	}

	// Outside the user's pools nothing schedules.
	outside := map[string]string{"pool": "c", "example.com/gpu": "a100"}
	for _, ds := range sets {
		if ds.matches(t, outside) {
			t.Errorf("%s schedules on %v, outside nodemon.affinity", ds.name, outside)
		}
	}
}

// Only the GPU DaemonSets carry the sidecar's privileges and mounts; the
// CPU-node DaemonSet keeps DCGM_HOST so nodemon probes localhost instead of
// listing pods through the API server on every GPU refresh.
func TestCPUNodeDaemonSetCarriesNoDCGM(t *testing.T) {
	for _, obj := range charttest.Objects(t, charttest.Render(t, platformSpec)) {
		if charttest.Describe(obj.Doc) != "DaemonSet/zxporter-nodemon" {
			continue
		}
		spec := obj.Doc["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
		if c := container(spec, "dcgm-exporter"); c != nil {
			t.Fatal("zxporter-nodemon carries a dcgm-exporter container")
		}
		rawVolumes, _ := spec["volumes"].([]any)
		volumes := make([]string, 0, len(rawVolumes))
		for _, v := range rawVolumes {
			volumes = append(volumes, v.(map[string]any)["name"].(string))
		}
		sort.Strings(volumes)
		if got := strings.Join(volumes, ","); got != "cgroup" {
			t.Errorf("volumes = %s, want cgroup only", got)
		}
		if got := env(container(spec, "zxporter-nodemon"), "DCGM_HOST"); got != "localhost" {
			t.Errorf("DCGM_HOST = %q, want localhost", got)
		}
		annotations := charttest.StringMap(obj.Doc, "metadata", "annotations")
		for key := range annotations {
			if strings.HasPrefix(key, "ignore-check.kube-linter.io/") || strings.HasPrefix(key, "devzero.io/gpu-runtime") {
				t.Errorf("unexpected DCGM annotation %s", key)
			}
		}
		return
	}
	t.Fatal("no DaemonSet/zxporter-nodemon rendered")
}

func TestGPUPlacementSettingsAreValidated(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"unknown mode", charttest.Set("dcgmExporter.mode=sometimes"), "dcgmExporter.mode"},
		// Gt/Lt have no exact negation: a node whose label is not an integer
		// would match neither the GPU nor the CPU DaemonSet.
		{"Gt operator", charttest.SetJSON("dcgmExporter.gpuNodeSelectors",
			`[{"key":"x","operator":"Gt","values":["0"]}]`), "unsupported operator"},
		{"In without values", charttest.SetJSON("dcgmExporter.gpuNodeSelectors",
			`[{"key":"x","operator":"In"}]`), "needs at least one value"},
		{"Exists with values", charttest.SetJSON("dcgmExporter.gpuNodeSelectors",
			`[{"key":"x","operator":"Exists","values":["a"]}]`), "takes no values"},
		{"entry without a key", charttest.SetJSON("dcgmExporter.gpuNodeSelectors",
			`[{"operator":"Exists"}]`), "need key and operator"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := charttest.TryRender(t, platformSpec, tc.args...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want an error containing %q, got %v", tc.want, err)
			}
		})
	}
}
