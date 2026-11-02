package charttest

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

// The image-registry contract the dz-platform umbrella relies on: with
// global.imageRegistry set, every image a chart renders comes from that
// registry, and with global.imagePullSecrets set, every pod carries those
// secrets. Like the metadata contract it sweeps every rendered pod template
// rather than a hand-listed one, so a new workload that skips the helpers fails
// here instead of silently pulling from a public registry.

// ImageGlobals describes how a chart takes part in the contract.
type ImageGlobals struct {
	// PullSecretsKey is the chart's own pull-secrets value key, merged with
	// global.imagePullSecrets (for example "imagePullSecrets").
	PullSecretsKey string
	// OwnChartOnly skips subchart objects, for a parent whose bundled
	// subchart is covered by its own package.
	OwnChartOnly bool
	// Variants are extra configurations, each rendering pod templates or
	// containers the default does not.
	Variants map[string][]string
}

// mirror is the registry the contract renders with. The path prefix proves
// the helper appends to the value rather than treating it as a bare host.
const mirror = "mirror.example/devzero"

// AssertImageGlobals checks the contract for the chart's default render and
// each variant.
func AssertImageGlobals(t *testing.T, spec Spec, g ImageGlobals) {
	t.Helper()
	variants := map[string][]string{"default": nil}
	for name, args := range g.Variants {
		variants[name] = args
	}
	for name, args := range variants {
		t.Run(name, func(t *testing.T) {
			assertImageGlobals(t, spec, g, args)
		})
	}
}

func assertImageGlobals(t *testing.T, spec Spec, g ImageGlobals, args []string) {
	t.Helper()
	render := func(extra ...string) map[string]map[string]any {
		all := append(append([]string{}, args...), extra...)
		return PodTemplates(Objects(t, Render(t, spec, all...)), g.OwnChartOnly)
	}

	defaults := render()
	if len(defaults) == 0 {
		t.Fatal("rendered no pod templates")
	}
	for name, tmpl := range defaults {
		if got := PullSecretNames(tmpl); len(got) != 0 {
			t.Errorf("%s: imagePullSecrets %v with no pull secrets set", name, got)
		}
	}

	mirrored := render("--set", "global.imageRegistry="+mirror)
	for name, tmpl := range defaults {
		want := Images(tmpl)
		for i := range want {
			want[i] = RewriteImage(mirror, want[i])
		}
		if got := Images(mirrored[name]); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: images with global.imageRegistry=%s\n got %v\nwant %v", name, mirror, got, want)
		}
	}

	withSecrets := render(
		"--set", "global.imagePullSecrets[0]=from-global",
		"--set", "global.imagePullSecrets[1].name=both",
		"--set", g.PullSecretsKey+"[0]=both",
		"--set", g.PullSecretsKey+"[1].name=own",
	)
	want := []string{"from-global", "both", "own"}
	for name := range defaults {
		if got := PullSecretNames(withSecrets[name]); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: imagePullSecrets = %v, want %v (global first, then %s, deduplicated)", name, got, want, g.PullSecretsKey)
		}
	}

	// Valid Secret names that YAML reads as a number or a bool must still
	// render as strings, or the API server rejects the pod.
	scalarLooking := render(
		"--set", "global.imagePullSecrets[0]=123",
		"--set-string", g.PullSecretsKey+"[0].name=true",
	)
	want = []string{"123", "true"}
	for name := range defaults {
		if got := PullSecretNames(scalarLooking[name]); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: imagePullSecrets = %v, want %v (scalar-looking names kept as strings)", name, got, want)
		}
	}
}

// Images lists a pod template's init container and container images, in order.
func Images(template map[string]any) []string {
	spec, _ := template["spec"].(map[string]any)
	var out []string
	for _, key := range []string{"initContainers", "containers"} {
		containers, _ := spec[key].([]any)
		for _, c := range containers {
			container, _ := c.(map[string]any)
			image, _ := container["image"].(string)
			out = append(out, image)
		}
	}
	return out
}

// PullSecretNames lists the names in a pod template's imagePullSecrets. An
// entry without a name (the bare-string shape the API rejects) is reported
// as "<invalid>".
func PullSecretNames(template map[string]any) []string {
	spec, _ := template["spec"].(map[string]any)
	secrets, _ := spec["imagePullSecrets"].([]any)
	out := make([]string, 0, len(secrets))
	for _, s := range secrets {
		entry, _ := s.(map[string]any)
		name, _ := entry["name"].(string)
		if name == "" {
			name = "<invalid>"
		}
		out = append(out, name)
	}
	return out
}

// RewriteImage is the reference rule devzero-common.imageRef implements,
// restated independently so the test does not trust the template it checks:
// keep the path, tag and digest, replace the registry host with registry, and
// spell out Docker Hub's implicit library/ namespace. A first path component
// is a host when it has a ".", a ":" or an uppercase letter, or is localhost.
func RewriteImage(registry, ref string) string {
	name, digest, _ := strings.Cut(ref, "@")
	if digest != "" {
		digest = "@" + digest
	}
	tag := ""
	if i := strings.LastIndex(name, ":"); i > strings.LastIndex(name, "/") {
		name, tag = name[:i], name[i:]
	}
	path, dockerHub := name, true
	if first, rest, found := strings.Cut(name, "/"); found &&
		(strings.ContainsAny(first, ".:") || first == "localhost" || strings.ToLower(first) != first) {
		path = rest
		dockerHub = slices.Contains([]string{"docker.io", "index.docker.io", "registry-1.docker.io"}, first)
	}
	if dockerHub && !strings.Contains(path, "/") {
		path = "library/" + path
	}
	return strings.TrimSuffix(registry, "/") + "/" + path + tag + digest
}
