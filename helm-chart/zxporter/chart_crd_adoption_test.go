package zxporter_test

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/devzero-inc/zxporter/helm-chart/internal/charttest"
)

// crdAdoptionJob is the hook Job as charttest.Describe names it.
const crdAdoptionJob = "Job/release-zxporter-crd-adopt"

// shippedCRDs reads the CRD names from crds/, the list the hook must cover.
func shippedCRDs(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("crds/*.yaml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no CRDs in crds/: %v", err)
	}
	var names []string
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var crd struct {
			Metadata struct{ Name string } `json:"metadata"`
		}
		if err := yaml.Unmarshal(raw, &crd); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		names = append(names, crd.Metadata.Name)
	}
	sort.Strings(names)
	return names
}

// hookObjects renders the chart and returns the CRD-adoption hook's objects by
// charttest.Describe name.
func hookObjects(t *testing.T, args ...string) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, obj := range charttest.Objects(t, charttest.Render(t, platformSpec, args...)) {
		if strings.HasSuffix(obj.Source, "templates/crd-adoption-job.yaml") {
			out[charttest.Describe(obj.Doc)] = obj.Doc
		}
	}
	return out
}

func adoptionScript(t *testing.T, job map[string]any) string {
	t.Helper()
	tmpl := charttest.PodTemplates([]charttest.Object{{Doc: job}}, false)[crdAdoptionJob]
	containers, _ := tmpl["spec"].(map[string]any)["containers"].([]any)
	if len(containers) != 1 {
		t.Fatalf("hook Job has %d containers, want 1", len(containers))
	}
	command, _ := containers[0].(map[string]any)["command"].([]any)
	if len(command) == 0 {
		t.Fatal("hook container has no command")
	}
	script, _ := command[len(command)-1].(string)
	return script
}

// The hook may touch exactly the CRDs crds/ ships, and patches every one of
// them with this release's ownership metadata and resource-policy keep. The
// list is derived from crds/, so a new CRD is covered without editing the hook.
func TestCRDAdoptionPatchesEveryShippedCRDWithThisRelease(t *testing.T) {
	want := shippedCRDs(t)
	objs := hookObjects(t, "--namespace", "dz-ns")

	role := objs["ClusterRole/release-zxporter-crd-adopt"]
	if role == nil {
		t.Fatalf("no hook ClusterRole; rendered %v", keys(objs))
	}
	rules, _ := role["rules"].([]any)
	if len(rules) != 1 {
		t.Fatalf("ClusterRole has %d rules, want 1", len(rules))
	}
	rule := rules[0].(map[string]any)
	var granted []string
	for _, n := range rule["resourceNames"].([]any) {
		granted = append(granted, n.(string))
	}
	sort.Strings(granted)
	if !reflect.DeepEqual(granted, want) {
		t.Errorf("ClusterRole resourceNames %v, want the shipped CRDs %v", granted, want)
	}
	if verbs := rule["verbs"]; !reflect.DeepEqual(verbs, []any{"get", "patch"}) {
		t.Errorf("ClusterRole verbs %v, want [get patch]", verbs)
	}

	script := adoptionScript(t, objs[crdAdoptionJob])
	if !strings.Contains(script, "for crd in "+strings.Join(want, " ")+";") {
		t.Errorf("script does not iterate the shipped CRDs %v:\n%s", want, script)
	}
	for _, fragment := range []string{
		`"app.kubernetes.io/managed-by":"Helm"`,
		`"meta.helm.sh/release-name":"release"`,
		`"meta.helm.sh/release-namespace":"dz-ns"`,
		`"helm.sh/resource-policy":"keep"`,
	} {
		if !strings.Contains(script, fragment) {
			t.Errorf("patch is missing %s:\n%s", fragment, script)
		}
	}

	for name, obj := range objs {
		hook := charttest.StringMap(obj, "metadata", "annotations")["helm.sh/hook"]
		if hook != "pre-install,pre-upgrade" {
			t.Errorf("%s: helm.sh/hook %q, want pre-install,pre-upgrade", name, hook)
		}
	}
}

// Switching the hook off renders none of it, including as the string an
// installer's --set passes.
func TestCRDAdoptionOffRendersNothing(t *testing.T) {
	for _, args := range [][]string{
		{"--set", "crdAdoption.enabled=false"},
		{"--set-string", "crdAdoption.enabled=false"},
	} {
		if objs := hookObjects(t, args...); len(objs) != 0 {
			t.Errorf("%v: rendered %v, want nothing", args, keys(objs))
		}
	}
}

// `helm upgrade --reuse-values` from a release that predates crdAdoption
// renders the hook as a fresh install would, with the same image.
func TestCRDAdoptionRunsWithValuesThatPredateIt(t *testing.T) {
	image := func(args ...string) []string {
		job := hookObjects(t, args...)[crdAdoptionJob]
		if job == nil {
			t.Fatalf("%v: no hook Job", args)
		}
		return charttest.Images(charttest.PodTemplates([]charttest.Object{{Doc: job}}, false)[crdAdoptionJob])
	}
	if got, want := image("--set", "crdAdoption=null"), image(); !reflect.DeepEqual(got, want) {
		t.Errorf("hook image without crdAdoption values %v, want the default %v", got, want)
	}
}

func keys(m map[string]map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
