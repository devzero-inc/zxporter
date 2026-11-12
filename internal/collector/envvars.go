package collector

import (
	"fmt"
	"strings"
)

// Values accepted by the ENV_VAR_COLLECTION setting (Helm value
// zxporter.envVarCollection, CollectionPolicy spec.policies.envVarCollection).
const (
	// EnvVarCollectionEnabled sends objects with their environment variables,
	// exactly as the Kubernetes API returns them. It is the default.
	EnvVarCollectionEnabled = "enabled"
	// EnvVarCollectionDisabled removes environment variables from every object
	// before it leaves the cluster. See StripEnvVars.
	EnvVarCollectionDisabled = "disabled"
)

// ParseEnvVarCollection reports whether env var collection is disabled for the
// given setting. Empty means the default (enabled); anything other than
// "enabled" or "disabled" (case-insensitive) is an error.
func ParseEnvVarCollection(value string) (disabled bool, err error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", EnvVarCollectionEnabled:
		return false, nil
	case EnvVarCollectionDisabled:
		return true, nil
	default:
		return false, fmt.Errorf(
			"invalid env var collection %q: must be %q or %q",
			value, EnvVarCollectionEnabled, EnvVarCollectionDisabled,
		)
	}
}

// envVarFields are the object keys that hold environment variables in the
// objects zxporter collects:
//
//   - env, envFrom: every core container (Pod and every pod template: Deployment,
//     StatefulSet, DaemonSet, ReplicaSet, Job, CronJob, ReplicationController,
//     Argo Rollout, Kubeflow Notebook, Volcano Job, ...), and the same names in
//     CRDs that model env directly (CNPG Cluster spec.env, DatadogAgent
//     overrides, Argo CD Application source.plugin.env).
//   - envVars, envSecretKeyRefs: Spark operator driver/executor specs.
//   - extraEnv, extraEnvVars: the common Helm-chart conventions, which reach
//     dakr inside Flux HelmRelease spec.values.
//
// A key is removed only when its value is a non-empty list or object: a scalar
// such as a label "env: prod" is kept, and so is an empty one, which holds no
// value (Volcano's spec.plugins.env: [] switches a plugin on).
var envVarFields = map[string]struct{}{
	"env":              {},
	"envFrom":          {},
	"envVars":          {},
	"envSecretKeyRefs": {},
	"extraEnv":         {},
	"extraEnvVars":     {},
}

// envDerivedFields are removed whatever their type, because their content
// carries env var names or values under another key:
//
//   - managedFields: server-side-apply bookkeeping lists every field by name,
//     env included (f:env → k:{"name":"DB_PASSWORD"}). Informers strip it,
//     but the cluster snapshot's refresh path lists objects straight from the
//     API.
//   - jvmRawCmdline: zxporter-nodemon appends the raw JAVA_TOOL_OPTIONS,
//     JDK_JAVA_OPTIONS and JAVA_OPTS values from /proc/<pid>/environ to the
//     JVM command line. The flags dakr sizes from (-Xmx, MaxRAMPercentage, ...)
//     are sent parsed in their own fields and are kept.
//   - the kubectl.kubernetes.io/last-applied-configuration annotation, a full
//     JSON copy of the applied object, env included. Informers already strip
//     it from their caches (StripMetadataTransform).
var envDerivedFields = map[string]struct{}{
	"managedFields":             {},
	"jvmRawCmdline":             {},
	lastAppliedConfigAnnotation: {},
}

// sparkEnvConfPrefixes are the Spark configuration keys that set an
// environment variable, one key per variable: sparkConf
// {"spark.executorEnv.DB_PASSWORD": "..."} on SparkApplication and
// ScheduledSparkApplication. Any map key with one of these prefixes is removed.
var sparkEnvConfPrefixes = []string{
	"spark.executorEnv.",
	"spark.kubernetes.driverEnv.",
	"spark.yarn.appMasterEnv.",
}

func isSparkEnvConfKey(key string) bool {
	for _, prefix := range sparkEnvConfPrefixes {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

// StripEnvVars removes environment variables, in place, from a JSON-decoded
// object (the map[string]interface{} / []interface{} tree encoding/json
// produces). It walks the whole tree, so it covers containers, init and
// ephemeral containers, pod templates nested at any depth, and CRDs, without a
// per-kind list that a new collector could miss.
//
// It also drops envDerivedFields and Spark env conf keys (sparkEnvConfPrefixes)
// wherever they appear.
//
// The caller must own data: StripEnvVars is meant for the private copy the
// transport decodes just before sending, never for an informer-cached object.
func StripEnvVars(data interface{}) {
	switch v := data.(type) {
	case map[string]interface{}:
		for key, child := range v {
			if _, ok := envDerivedFields[key]; ok || isSparkEnvConfKey(key) {
				delete(v, key)
				continue
			}
			if _, ok := envVarFields[key]; ok {
				switch c := child.(type) {
				case []interface{}:
					if len(c) > 0 {
						delete(v, key)
						continue
					}
				case map[string]interface{}:
					if len(c) > 0 {
						delete(v, key)
						continue
					}
				}
			}
			StripEnvVars(child)
		}
	case []interface{}:
		for _, child := range v {
			StripEnvVars(child)
		}
	}
}
