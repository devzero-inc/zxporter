package collector

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// decode round-trips obj through JSON the way the transport does before
// sending, so the tests strip exactly the tree dakr would receive.
func decode(t *testing.T, obj any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(obj)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	return m
}

func encode(t *testing.T, m map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(m)
	require.NoError(t, err)
	return string(raw)
}

func envContainer(name string) corev1.Container {
	return corev1.Container{
		Name:  name,
		Image: "registry.example/" + name + ":1.0",
		Env: []corev1.EnvVar{
			{Name: "DB_PASSWORD", Value: "s3cr3t-" + name},
			{Name: "API_KEY", ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "keyref-" + name},
					Key:                  "key",
				},
			}},
		},
		EnvFrom: []corev1.EnvFromSource{{
			ConfigMapRef: &corev1.ConfigMapEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: "envfrom-" + name},
			},
		}},
	}
}

func TestStripEnvVars_RemovesEnvFromEveryContainerOfAPodTemplate(t *testing.T) {
	dep := &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{Kind: "Deployment", APIVersion: "apps/v1"},
		ObjectMeta: metav1.ObjectMeta{
			Name:        "web",
			Namespace:   "default",
			Labels:      map[string]string{"env": "prod"},
			Annotations: map[string]string{"team": "payments"},
		},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				lastAppliedConfigAnnotation: `{"env":[{"name":"X","value":"s3cr3t-applied"}]}`,
			}},
			Spec: corev1.PodSpec{
				InitContainers: []corev1.Container{envContainer("init")},
				Containers:     []corev1.Container{envContainer("app"), envContainer("sidecar")},
			},
		}},
	}

	m := decode(t, dep)
	StripEnvVars(m)
	got := encode(t, m)

	for _, gone := range []string{"s3cr3t-", "keyref-", "envfrom-", "DB_PASSWORD", "API_KEY", `"envFrom"`} {
		assert.NotContains(t, got, gone)
	}
	// Everything that is not an env var still reaches dakr: names, images, and a
	// scalar label that happens to be called "env".
	for _, kept := range []string{
		`"name":"init"`, `"name":"app"`, `"name":"sidecar"`,
		"registry.example/app:1.0", `"env":"prod"`, `"team":"payments"`,
	} {
		assert.Contains(t, got, kept)
	}
}

// CRDs carry env vars under their own shapes; the walk must find each one
// without a per-kind list.
func TestStripEnvVars_RemovesEnvFromCRDShapes(t *testing.T) {
	cases := []struct {
		name string
		obj  map[string]any
		kept string
	}{
		{
			name: "spark driver envVars map, envSecretKeyRefs and executor env",
			obj: map[string]any{"spec": map[string]any{
				"driver": map[string]any{
					"cores":            1,
					"envVars":          map[string]any{"TOKEN": "s3cr3t-spark"},
					"envSecretKeyRefs": map[string]any{"PW": map[string]any{"name": "s3cr3t-ref", "key": "k"}},
				},
				"executor": map[string]any{"env": []any{map[string]any{"name": "A", "value": "s3cr3t-exec"}}},
			}},
			kept: `"cores":1`,
		},
		{
			// Both Spark collectors send the whole object under "raw"; a
			// ScheduledSparkApplication nests it under spec.template.
			name: "spark sparkConf env keys, scheduled template under raw",
			obj: map[string]any{"raw": map[string]any{"spec": map[string]any{"template": map[string]any{
				"sparkConf": map[string]any{
					"spark.executorEnv.DB_PASSWORD":           "s3cr3t-exec-conf",
					"spark.kubernetes.driverEnv.API_KEY":      "s3cr3t-driver-conf",
					"spark.yarn.appMasterEnv.TOKEN":           "s3cr3t-am-conf",
					"spark.kubernetes.executor.request.cores": "2",
				},
			}}}},
			kept: `"spark.kubernetes.executor.request.cores":"2"`,
		},
		{
			name: "cnpg cluster spec.env and spec.envFrom",
			obj: map[string]any{"spec": map[string]any{
				"instances": 3,
				"env":       []any{map[string]any{"name": "PGPASS", "value": "s3cr3t-cnpg"}},
				"envFrom":   []any{map[string]any{"secretRef": map[string]any{"name": "s3cr3t-cnpgref"}}},
			}},
			kept: `"instances":3`,
		},
		{
			name: "argo cd application source.plugin.env",
			obj: map[string]any{"spec": map[string]any{"source": map[string]any{
				"repoURL": "https://git.example/repo",
				"plugin":  map[string]any{"env": []any{map[string]any{"name": "K", "value": "s3cr3t-argo"}}},
			}}},
			kept: "https://git.example/repo",
		},
		{
			name: "flux helmrelease values extraEnv and extraEnvVars",
			obj: map[string]any{"spec": map[string]any{"values": map[string]any{
				"replicaCount": 2,
				"extraEnv":     []any{map[string]any{"name": "K", "value": "s3cr3t-flux"}},
				"extraEnvVars": map[string]any{"K2": "s3cr3t-flux2"},
			}}},
			kept: `"replicaCount":2`,
		},
		{
			name: "managedFields from an API list (snapshot refresh) name every env var",
			obj: map[string]any{"metadata": map[string]any{
				"name": "web",
				"managedFields": []any{map[string]any{"fieldsV1": map[string]any{
					"f:spec": map[string]any{"f:env": map[string]any{`k:{"name":"s3cr3t-name"}`: map[string]any{}}},
				}}},
			}},
			kept: `"name":"web"`,
		},
		{
			name: "container metrics jvmRawCmdline carries JAVA_TOOL_OPTIONS",
			obj: map[string]any{
				"jvmXmxBytes":   536870912,
				"jvmRawCmdline": "java -jar app.jar  -Dpassword=s3cr3t-jvm",
			},
			kept: `"jvmXmxBytes":536870912`,
		},
		{
			name: "pod ephemeral container",
			obj: decode(t, &corev1.Pod{Spec: corev1.PodSpec{
				EphemeralContainers: []corev1.EphemeralContainer{{
					EphemeralContainerCommon: corev1.EphemeralContainerCommon(envContainer("debug")),
				}},
			}}),
			kept: `"name":"debug"`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			StripEnvVars(tc.obj)
			got := encode(t, tc.obj)
			assert.NotContains(t, got, "s3cr3t")
			assert.Contains(t, got, tc.kept)
		})
	}
}

func TestParseEnvVarCollection(t *testing.T) {
	cases := []struct {
		in           string
		wantDisabled bool
		wantErr      bool
	}{
		{in: "", wantDisabled: false},
		{in: "enabled", wantDisabled: false},
		{in: "disabled", wantDisabled: true},
		{in: " Disabled ", wantDisabled: true},
		{in: "false", wantErr: true},
		{in: "off", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			disabled, err := ParseEnvVarCollection(tc.in)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantDisabled, disabled)
		})
	}
}

// An empty env holds no value and keeps meaning elsewhere: Volcano's
// spec.plugins.env: [] switches its env plugin on.
func TestStripEnvVars_KeepsEmptyEnv(t *testing.T) {
	obj := map[string]any{"spec": map[string]any{"plugins": map[string]any{"env": []any{}, "svc": []any{}}}}
	StripEnvVars(obj)
	assert.Equal(t, `{"spec":{"plugins":{"env":[],"svc":[]}}}`, encode(t, obj))
}
