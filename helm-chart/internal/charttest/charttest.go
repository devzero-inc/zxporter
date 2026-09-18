// Package charttest holds the shared assertions for the site-metadata contract
// (commonLabels, commonAnnotations, podLabels, podAnnotations) that every chart
// in this directory implements.
//
// Clusters running an admission policy engine (OPA Gatekeeper, Kyverno) commonly
// require site-specific labels on every object before they will admit it. A
// chart with no way to set them cannot be installed there at all, so these
// assertions are what keep the knobs working as templates change.
//
// The assertions sweep every rendered object rather than a hand-listed sample.
// That matters more than it sounds: the equivalent contract was first written
// with a hand-maintained list of chart-owned keys, and the list was wrong twice.
// Anything derived from a render cannot drift from the templates.
package charttest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	yamlv3 "gopkg.in/yaml.v3"
	"sigs.k8s.io/yaml"
)

// Spec describes one chart under test.
type Spec struct {
	// ChartDir is the chart directory, relative to the test file.
	ChartDir string
	// BaseArgs are the --set flags this chart needs in order to render at all.
	BaseArgs []string
	// SubchartPrefix, when set, is a rendered-source prefix that must appear in
	// the output, proving the parent's global values reached that subchart.
	SubchartPrefix string
	// Variants are additional configurations the whole contract is re-run
	// against, named for the subtest.
	//
	// A chart that renders a metadata block only under some condition satisfies
	// the contract in the default render and breaks it elsewhere — the nodemon
	// DaemonSet put its annotations inside `if $v.withDcgm`, so turning DCGM off
	// dropped commonAnnotations entirely while the default render stayed green.
	Variants map[string][]string
}

// withArgs returns a copy of the spec carrying extra base arguments.
func (s Spec) withArgs(extra []string) Spec {
	out := s
	out.BaseArgs = append(append([]string{}, s.BaseArgs...), extra...)
	out.Variants = nil
	return out
}

// dependencyBuild runs once per chart directory. A chart with a file:// subchart
// dependency cannot render until charts/ is populated, and charts/*.tgz is
// gitignored, so a clean checkout has none. Doing it here keeps `go test
// ./helm-chart/...` working without a separate setup step.
var dependencyBuild sync.Map

func ensureDependencies(t *testing.T, chartDir string) {
	t.Helper()

	if _, loaded := dependencyBuild.LoadOrStore(chartDir, true); loaded {
		return
	}
	if _, err := os.Stat(filepath.Join(chartDir, "Chart.lock")); err != nil {
		return // no dependencies to build
	}

	cmd := exec.Command("helm", "dependency", "build", chartDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("helm dependency build failed for %s: %v\n%s", chartDir, err, out)
	}
}

// Render runs `helm template` and returns the rendered manifests.
func Render(t *testing.T, spec Spec, extraArgs ...string) string {
	t.Helper()

	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm binary not available, skipping chart render test")
	}

	chartDir, err := filepath.Abs(spec.ChartDir)
	if err != nil {
		t.Fatalf("resolving chart dir: %v", err)
	}
	ensureDependencies(t, chartDir)

	args := append([]string{"template", "release", chartDir}, spec.BaseArgs...)
	args = append(args, extraArgs...)

	var stdout, stderr bytes.Buffer
	cmd := exec.Command("helm", args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("helm template failed: %v\n%s", err, stderr.String())
	}
	return stdout.String()
}

// Object is one rendered document plus the chart path Helm attributed it to.
type Object struct {
	// Source is the `# Source:` path Helm emits above each document.
	Source string
	Doc    map[string]any
}

// FromSubchart reports whether Helm rendered this object from a subchart.
//
// Helm values are chart-scoped: a chart-local commonLabels applies to the chart
// it is set on and does not cross into a subchart, which is what global.* is
// for. Assertions about a chart-local value therefore have to skip these, while
// the global assertion covers every object.
func (o Object) FromSubchart() bool {
	return strings.Contains(o.Source, "/charts/")
}

// Objects decodes every document in a rendered manifest stream.
func Objects(t *testing.T, manifests string) []Object {
	t.Helper()

	docs := strings.Split(manifests, "\n---\n")
	objects := make([]Object, 0, len(docs))
	for _, doc := range docs {
		if strings.TrimSpace(doc) == "" {
			continue
		}

		var obj map[string]any
		if err := yaml.Unmarshal([]byte(doc), &obj); err != nil {
			t.Fatalf("unmarshalling rendered manifest: %v\n%s", err, doc)
		}
		if obj == nil || obj["kind"] == nil {
			continue
		}

		source := ""
		for _, line := range strings.Split(doc, "\n") {
			if rest, found := strings.CutPrefix(strings.TrimSpace(line), "# Source:"); found {
				source = strings.TrimSpace(rest)
				break
			}
		}
		objects = append(objects, Object{Source: source, Doc: obj})
	}

	if len(objects) == 0 {
		t.Fatal("rendered no objects")
	}
	return objects
}

// Describe identifies an object in assertion failures.
func Describe(obj map[string]any) string {
	kind, _ := obj["kind"].(string)
	meta, _ := obj["metadata"].(map[string]any)
	name, _ := meta["name"].(string)
	return kind + "/" + name
}

// StringMap reads a nested map[string]string, e.g. metadata.labels.
func StringMap(obj map[string]any, path ...string) map[string]string {
	cur := obj
	for _, key := range path[:len(path)-1] {
		next, ok := cur[key].(map[string]any)
		if !ok {
			return nil
		}
		cur = next
	}

	raw, ok := cur[path[len(path)-1]].(map[string]any)
	if !ok {
		return nil
	}
	out := map[string]string{}
	for k, v := range raw {
		s, _ := v.(string)
		out[k] = s
	}
	return out
}

// PodTemplates returns the spec.template of every workload object. When
// ownChartOnly is set, subchart objects are skipped.
func PodTemplates(objects []Object, ownChartOnly bool) map[string]map[string]any {
	templates := map[string]map[string]any{}
	for _, obj := range objects {
		if ownChartOnly && obj.FromSubchart() {
			continue
		}
		spec, ok := obj.Doc["spec"].(map[string]any)
		if !ok {
			continue
		}
		template, ok := spec["template"].(map[string]any)
		if !ok {
			continue
		}
		if _, ok := template["spec"].(map[string]any); !ok {
			continue
		}
		templates[Describe(obj.Doc)] = template
	}
	return templates
}

// SetJSON builds a --set-json argument pair.
func SetJSON(key, jsonValue string) []string {
	return []string{"--set-json", key + "=" + jsonValue}
}

// DuplicateKeys reports every mapping in the stream that defines the same key
// twice. sigs.k8s.io/yaml routes through JSON and silently keeps the last value,
// so a duplicate is invisible to the other assertions here — but the API server
// rejects it under strict decoding, which is how a user would find out.
func DuplicateKeys(t *testing.T, manifests string) []string {
	t.Helper()

	var found []string
	var walk func(node *yamlv3.Node, path string)
	walk = func(node *yamlv3.Node, path string) {
		if node.Kind == yamlv3.MappingNode {
			seen := map[string]bool{}
			for i := 0; i+1 < len(node.Content); i += 2 {
				key := node.Content[i].Value
				if seen[key] {
					found = append(found, fmt.Sprintf("%s.%s (line %d)", path, key, node.Content[i].Line))
				}
				seen[key] = true
				walk(node.Content[i+1], path+"."+key)
			}
			return
		}
		for _, child := range node.Content {
			walk(child, path)
		}
	}

	decoder := yamlv3.NewDecoder(strings.NewReader(manifests))
	for {
		var doc yamlv3.Node
		err := decoder.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decoding rendered manifests: %v", err)
		}
		walk(&doc, "")
	}
	return found
}

// emittedKeys harvests every metadata key the chart emits on its own, from a
// render with the knobs unset.
func emittedKeys(t *testing.T, spec Spec) (labels, annotations []string) {
	t.Helper()

	labelSet := map[string]bool{}
	annotationSet := map[string]bool{}
	collect := func(meta map[string]any) {
		for key := range StringMap(meta, "metadata", "labels") {
			labelSet[key] = true
		}
		for key := range StringMap(meta, "metadata", "annotations") {
			annotationSet[key] = true
		}
	}

	objects := Objects(t, Render(t, spec))
	for _, obj := range objects {
		collect(obj.Doc)
	}
	for _, template := range PodTemplates(objects, false) {
		collect(template)
	}

	for key := range labelSet {
		labels = append(labels, key)
	}
	for key := range annotationSet {
		annotations = append(annotations, key)
	}
	sort.Strings(labels)
	sort.Strings(annotations)
	return labels, annotations
}

func hostileMap(keys []string) string {
	pairs := make([]string, 0, len(keys))
	for _, key := range keys {
		pairs = append(pairs, fmt.Sprintf("%q:%q", key, "hijacked"))
	}
	return "{" + strings.Join(pairs, ",") + "}"
}

// selectors returns every selector in the render, keyed by object. Selectors on
// Deployments, DaemonSets and StatefulSets are immutable after creation, so
// changing one turns `helm upgrade` into a manual delete-and-recreate for every
// existing installation.
func selectors(t *testing.T, objects []Object) map[string]string {
	t.Helper()

	found := map[string]string{}
	for _, obj := range objects {
		spec, ok := obj.Doc["spec"].(map[string]any)
		if !ok {
			continue
		}
		selector, ok := spec["selector"]
		if !ok {
			continue
		}
		encoded, err := json.Marshal(selector)
		if err != nil {
			t.Fatalf("marshalling selector for %s: %v", Describe(obj.Doc), err)
		}
		found[Describe(obj.Doc)] = string(encoded)
	}
	return found
}

// RunContract asserts the full site-metadata contract for one chart, and for
// each of its Variants.
func RunContract(t *testing.T, spec Spec) {
	t.Helper()

	runContract(t, spec)

	for name, args := range spec.Variants {
		t.Run(name, func(t *testing.T) { runContract(t, spec.withArgs(args)) })
	}
}

func runContract(t *testing.T, spec Spec) {
	t.Helper()

	for _, assertion := range []struct {
		name string
		run  func(*testing.T, Spec)
	}{
		{"common labels reach every object", assertCommonLabelsReachEveryObject},
		{"common annotations reach every object", assertCommonAnnotationsReachEveryObject},
		{"common metadata reaches every pod template", assertCommonMetadataReachesEveryPodTemplate},
		{"pod metadata reaches every pod template", assertPodMetadataReachesEveryPodTemplate},
		{"global common labels reach every object", assertGlobalCommonLabelsReachEveryObject},
		{"no chart-emitted key can produce a duplicate", assertNoChartemittedKeyCanProduceADuplicate},
		{"selectors are unchanged", assertSelectorsAreUnchanged},
		{"no empty metadata blocks when unset", assertNoEmptyMetadataBlocksWhenUnset},
	} {
		t.Run(assertion.name, func(t *testing.T) { assertion.run(t, spec) })
	}
}

func assertCommonLabelsReachEveryObject(t *testing.T, spec Spec) {
	t.Helper()

	objects := Objects(t, Render(t, spec,
		SetJSON("commonLabels", `{"example.com/cost-center":"platform","example.com/tier":"infra"}`)...))

	for _, obj := range objects {
		labels := StringMap(obj.Doc, "metadata", "labels")
		for key, want := range map[string]string{
			"example.com/cost-center": "platform",
			"example.com/tier":        "infra",
		} {
			// A chart-local value must reach this chart's own objects and must
			// NOT cross into a subchart — asserting only the first half would
			// let a regression that forwards it everywhere pass unnoticed.
			if obj.FromSubchart() {
				if got, found := labels[key]; found {
					t.Errorf("%s: chart-local commonLabels[%q] = %q leaked into a subchart", Describe(obj.Doc), key, got)
				}
				continue
			}
			if got := labels[key]; got != want {
				t.Errorf("%s: metadata.labels[%q] = %q, want %q", Describe(obj.Doc), key, got, want)
			}
		}
	}
}

func assertCommonAnnotationsReachEveryObject(t *testing.T, spec Spec) {
	t.Helper()

	objects := Objects(t, Render(t, spec,
		SetJSON("commonAnnotations", `{"example.com/owner":"platform-team"}`)...))

	for _, obj := range objects {
		if obj.FromSubchart() {
			continue // chart-local values do not cross into a subchart
		}
		if got := StringMap(obj.Doc, "metadata", "annotations")["example.com/owner"]; got != "platform-team" {
			t.Errorf("%s: metadata.annotations[example.com/owner] = %q, want platform-team", Describe(obj.Doc), got)
		}
	}
}

// Gatekeeper's expand-workloads evaluates constraints against the Pod a
// workload would create, so an object whose own metadata satisfies a policy
// can still be denied on its pod template.
func assertCommonMetadataReachesEveryPodTemplate(t *testing.T, spec Spec) {
	t.Helper()

	args := append(
		SetJSON("commonLabels", `{"example.com/tier":"infra"}`),
		SetJSON("commonAnnotations", `{"example.com/owner":"platform-team"}`)...,
	)
	templates := PodTemplates(Objects(t, Render(t, spec, args...)), true)
	if len(templates) == 0 {
		t.Fatal("rendered no pod templates")
	}

	for name, template := range templates {
		if got := StringMap(template, "metadata", "labels")["example.com/tier"]; got != "infra" {
			t.Errorf("%s: pod template labels[example.com/tier] = %q, want infra", name, got)
		}
		if got := StringMap(template, "metadata", "annotations")["example.com/owner"]; got != "platform-team" {
			t.Errorf("%s: pod template annotations[example.com/owner] = %q, want platform-team", name, got)
		}
	}
}

func assertPodMetadataReachesEveryPodTemplate(t *testing.T, spec Spec) {
	t.Helper()

	args := append(
		SetJSON("podLabels", `{"example.com/workload":"zxporter"}`),
		SetJSON("podAnnotations", `{"example.com/scrape":"false"}`)...,
	)
	objects := Objects(t, Render(t, spec, args...))

	templates := PodTemplates(objects, true)
	if len(templates) == 0 {
		t.Fatal("rendered no pod templates")
	}
	for name, template := range templates {
		if got := StringMap(template, "metadata", "labels")["example.com/workload"]; got != "zxporter" {
			t.Errorf("%s: pod template labels[example.com/workload] = %q, want zxporter", name, got)
		}
		if got := StringMap(template, "metadata", "annotations")["example.com/scrape"]; got != "false" {
			t.Errorf("%s: pod template annotations[example.com/scrape] = %q, want false", name, got)
		}
	}

	// podLabels is pod-scoped; leaking it onto object metadata would be
	// wrong for the knob's contract.
	for _, obj := range objects {
		if _, found := StringMap(obj.Doc, "metadata", "labels")["example.com/workload"]; found {
			t.Errorf("%s: podLabels leaked onto metadata.labels", Describe(obj.Doc))
		}
	}
}

// global.* is the knob that reaches a subchart; a chart-local commonLabels
// on the parent does not.
func assertGlobalCommonLabelsReachEveryObject(t *testing.T, spec Spec) {
	t.Helper()

	objects := Objects(t, Render(t, spec,
		SetJSON("global.commonLabels", `{"example.com/tier":"infra"}`)...))

	// Match the named prefix, not merely "some subchart rendered": a typo in
	// SubchartPrefix would otherwise still satisfy the guard below and leave the
	// propagation claim unproven for the subchart it names.
	sawNamedSubchart := false
	for _, obj := range objects {
		if spec.SubchartPrefix != "" && strings.HasPrefix(obj.Source, spec.SubchartPrefix) {
			sawNamedSubchart = true
		}
		if got := StringMap(obj.Doc, "metadata", "labels")["example.com/tier"]; got != "infra" {
			t.Errorf("%s: metadata.labels[example.com/tier] = %q, want infra", Describe(obj.Doc), got)
		}
	}
	if spec.SubchartPrefix != "" && !sawNamedSubchart {
		t.Errorf("no rendered object came from %q; without one this proves nothing about propagation",
			spec.SubchartPrefix)
	}
}

// The reserved key lists in the chart helpers are hand-maintained, so this
// derives the real set from a render instead of restating it: a template that
// starts emitting a new label fails here until that key is dropped, rather
// than shipping a chart that renders invalid YAML for anyone who sets it.
func assertNoChartemittedKeyCanProduceADuplicate(t *testing.T, spec Spec) {
	t.Helper()

	labels, annotations := emittedKeys(t, spec)
	if len(labels) == 0 {
		t.Fatal("harvested no chart-emitted label keys")
	}

	var args []string
	args = append(args, SetJSON("commonLabels", hostileMap(labels))...)
	args = append(args, SetJSON("podLabels", hostileMap(labels))...)
	args = append(args, SetJSON("global.commonLabels", hostileMap(labels))...)
	if len(annotations) > 0 {
		args = append(args, SetJSON("commonAnnotations", hostileMap(annotations))...)
		args = append(args, SetJSON("podAnnotations", hostileMap(annotations))...)
	}

	for _, dup := range DuplicateKeys(t, Render(t, spec, args...)) {
		t.Errorf("duplicate YAML key in rendered output: %s", dup)
	}
}

func assertSelectorsAreUnchanged(t *testing.T, spec Spec) {
	t.Helper()

	baseline := selectors(t, Objects(t, Render(t, spec)))
	if len(baseline) == 0 {
		t.Fatal("rendered no selectors")
	}

	args := append(
		SetJSON("commonLabels", `{"example.com/tier":"infra"}`),
		SetJSON("podLabels", `{"example.com/workload":"zxporter"}`)...,
	)
	withLabels := selectors(t, Objects(t, Render(t, spec, args...)))

	for name, want := range baseline {
		got, ok := withLabels[name]
		if !ok {
			t.Errorf("%s: selector disappeared when commonLabels was set", name)
			continue
		}
		if got != want {
			t.Errorf("%s: selector changed when commonLabels was set:\n got %s\nwant %s", name, got, want)
		}
	}
}

// With the knobs unset the chart must render exactly as it did before they
// existed, so adopting them is a no-op upgrade for every existing release.
func assertNoEmptyMetadataBlocksWhenUnset(t *testing.T, spec Spec) {
	t.Helper()

	for _, obj := range Objects(t, Render(t, spec)) {
		meta, _ := obj.Doc["metadata"].(map[string]any)
		if labels, ok := meta["labels"].(map[string]any); ok && len(labels) == 0 {
			t.Errorf("%s: rendered an empty metadata.labels block", Describe(obj.Doc))
		}
		if annotations, ok := meta["annotations"].(map[string]any); ok && len(annotations) == 0 {
			t.Errorf("%s: rendered an empty metadata.annotations block", Describe(obj.Doc))
		}
	}
}
