package collector

// legacyCollectorTypeAliases maps collector type names that older zxporter versions used
// as registry keys (and, for "statefulset" and "daemonset", documented as
// DisabledCollectors values) to the ResourceType a collector is registered under today.
//
// Those collectors' GetType() used to return these spellings, so a value written against
// them in a CollectionPolicy's disabledCollectors or in DISABLED_COLLECTORS must keep
// working. CanonicalCollectorType is the single place they are translated.
var legacyCollectorTypeAliases = map[string]ResourceType{
	"statefulset":           StatefulSet,
	"daemonset":             DaemonSet,
	"persistentvolume":      PersistentVolume,
	"persistentvolumeclaim": PersistentVolumeClaim,
	"crd":                   CustomResourceDefinition,
	"scaledjob":             KedaScaledJob,
	"scaledobject":          KedaScaledObject,
}

// CanonicalCollectorType returns the registry key for a user-supplied collector type,
// translating a legacy spelling; any other value is returned unchanged.
func CanonicalCollectorType(name string) string {
	if resourceType, ok := legacyCollectorTypeAliases[name]; ok {
		return resourceType.String()
	}
	return name
}

// CanonicalCollectorTypes applies CanonicalCollectorType to every entry, dropping
// duplicates that normalization creates (e.g. both "daemonset" and "daemon_set").
// It returns nil for an empty input so an unset list stays unset.
func CanonicalCollectorTypes(names []string) []string {
	if len(names) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(names))
	out := make([]string, 0, len(names))
	for _, name := range names {
		canonical := CanonicalCollectorType(name)
		if seen[canonical] {
			continue
		}
		seen[canonical] = true
		out = append(out, canonical)
	}
	return out
}
