package collector

import (
	"fmt"
	"sort"
	"strings"

	v2 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// isResourceTypeUnavailableError checks if the error indicates the resource type is not available
func isResourceTypeUnavailableError(err error) bool {
	errorMsg := err.Error()
	unavailableIndicators := []string{
		"the server could not find the requested resource",
		"no matches for kind",
		"resource not found",
		"not found",
	}

	for _, indicator := range unavailableIndicators {
		if contains(errorMsg, indicator) {
			return true
		}
	}

	return false
}

// contains is a simple string contains helper
func contains(s, substr string) bool {
	return len(s) >= len(substr) && s[0:][0:len(substr)] == substr
}

func subtractStrings(a, b []string) []string {
	toRemove := make(map[string]struct{})
	for _, item := range b {
		toRemove[item] = struct{}{}
	}

	var result []string
	for _, item := range a {
		if _, found := toRemove[item]; !found {
			result = append(result, item)
		}
	}

	return result
}

// Helper functions
func calculatePercentage(used, total int64) float64 {
	if total == 0 {
		return 0
	}
	return float64(used) / float64(total) * 100
}

func containsString(s, substr string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(substr))
}

// humanizeBytes converts bytes to a human-readable format (KB, MB, GB, etc.)
func humanizeBytes(bytes float64) string {
	const unit = 1024.0
	if bytes < unit {
		return fmt.Sprintf("%.2f B", bytes)
	}
	div, exp := unit, 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %ciB", bytes/div, "KMGTPE"[exp])
}

// timePointerEqual safely compares two time.Time pointers
func timePointerEqual(t1, t2 *v1.Time) bool {
	if t1 == nil && t2 == nil {
		return true
	}

	if t1 == nil || t2 == nil {
		return false
	}

	return t1.Equal(t2)
}

// int64PointerEqual safely compares two int64 pointers
func int64PointerEqual(i1, i2 *int64) bool {
	if i1 == nil && i2 == nil {
		return true
	}

	if i1 == nil || i2 == nil {
		return false
	}

	return *i1 == *i2
}

// int32PointerEqual safely compares two int32 pointers
func int32PointerEqual(i1, i2 *int32) bool {
	if i1 == nil && i2 == nil {
		return true
	}

	if i1 == nil || i2 == nil {
		return false
	}

	return *i1 == *i2
}

// getBoolValue safely gets the value of a bool pointer
func getBoolValue(b *bool) bool {
	if b == nil {
		return false
	}
	return *b
}

// finalizerSlicesEqual compares two FinalizerName slices for equality
func finalizerSlicesEqual(s1, s2 []v2.FinalizerName) bool {
	if len(s1) != len(s2) {
		return false
	}

	for i := range s1 {
		if s1[i] != s2[i] {
			return false
		}
	}

	return true
}

// nodeAffinityEqual compares two node affinities for equality
func nodeAffinityEqual(affinity1, affinity2 *v2.VolumeNodeAffinity) bool {
	if affinity1 == nil && affinity2 == nil {
		return true
	}

	if affinity1 == nil || affinity2 == nil {
		return false
	}

	// If there's no required field, they're equal
	if affinity1.Required == nil && affinity2.Required == nil {
		return true
	}

	if affinity1.Required == nil || affinity2.Required == nil {
		return false
	}

	// Compare the node selector terms
	terms1 := affinity1.Required.NodeSelectorTerms
	terms2 := affinity2.Required.NodeSelectorTerms

	if len(terms1) != len(terms2) {
		return false
	}

	// This is a simplistic comparison that assumes the order of terms matters
	// A more robust implementation would handle reordering of equivalent terms
	for i, term1 := range terms1 {
		term2 := terms2[i]

		// Compare match expressions
		if len(term1.MatchExpressions) != len(term2.MatchExpressions) {
			return false
		}

		for j, expr1 := range term1.MatchExpressions {
			expr2 := term2.MatchExpressions[j]

			if expr1.Key != expr2.Key ||
				expr1.Operator != expr2.Operator ||
				!stringSlicesEqual(expr1.Values, expr2.Values) {
				return false
			}
		}

		// Compare match fields
		if len(term1.MatchFields) != len(term2.MatchFields) {
			return false
		}

		for j, field1 := range term1.MatchFields {
			field2 := term2.MatchFields[j]

			if field1.Key != field2.Key ||
				field1.Operator != field2.Operator ||
				!stringSlicesEqual(field1.Values, field2.Values) {
				return false
			}
		}
	}

	return true
}

// stringSlicesEqual compares two string slices for equality
func stringSlicesEqual(s1, s2 []string) bool {
	if len(s1) != len(s2) {
		return false
	}

	// Create maps for efficient lookup
	map1 := make(map[string]bool)
	map2 := make(map[string]bool)

	for _, s := range s1 {
		map1[s] = true
	}

	for _, s := range s2 {
		map2[s] = true
	}

	// Compare the maps
	for s := range map1 {
		if !map2[s] {
			return false
		}
	}

	for s := range map2 {
		if !map1[s] {
			return false
		}
	}

	return true
}

// metaLabelsEqual compares two label selectors for equality
func metaLabelsEqual(s1, s2 *v1.LabelSelector) bool {
	if s1 == nil && s2 == nil {
		return true
	}

	if s1 == nil || s2 == nil {
		return false
	}

	// Check match labels
	if !mapsEqual(s1.MatchLabels, s2.MatchLabels) {
		return false
	}

	// Check match expressions
	if len(s1.MatchExpressions) != len(s2.MatchExpressions) {
		return false
	}

	// Create a string representation of each expression for comparison
	// This is simpler than comparing each field individually
	expr1 := make([]string, len(s1.MatchExpressions))
	expr2 := make([]string, len(s2.MatchExpressions))

	for i, exp := range s1.MatchExpressions {
		expr1[i] = fmt.Sprintf("%s-%s-%v", exp.Key, exp.Operator, exp.Values)
	}

	for i, exp := range s2.MatchExpressions {
		expr2[i] = fmt.Sprintf("%s-%s-%v", exp.Key, exp.Operator, exp.Values)
	}

	// Sort both slices to ensure consistent comparison
	sort.Strings(expr1)
	sort.Strings(expr2)

	// Compare the sorted expressions
	for i := range expr1 {
		if expr1[i] != expr2[i] {
			return false
		}
	}

	return true
}

// mapsEqual compares two maps for equality
func mapsEqual(m1, m2 map[string]string) bool {
	if len(m1) != len(m2) {
		return false
	}

	for k, v1 := range m1 {
		if v2, ok := m2[k]; !ok || v1 != v2 {
			return false
		}
	}

	return true
}

// objectReferencesEqual compares two slices of object references for equality
func objectReferencesEqual(refs1, refs2 []v2.ObjectReference) bool {
	if len(refs1) != len(refs2) {
		return false
	}

	// Create maps for efficient lookup
	refsMap1 := make(map[string]bool)
	refsMap2 := make(map[string]bool)

	for _, ref := range refs1 {
		key := fmt.Sprintf("%s/%s", ref.Namespace, ref.Name)
		refsMap1[key] = true
	}

	for _, ref := range refs2 {
		key := fmt.Sprintf("%s/%s", ref.Namespace, ref.Name)
		refsMap2[key] = true
	}

	// Compare maps
	for key := range refsMap1 {
		if !refsMap2[key] {
			return false
		}
	}

	for key := range refsMap2 {
		if !refsMap1[key] {
			return false
		}
	}

	return true
}

// boolPointerEqual compares two bool pointers for equality
func boolPointerEqual(b1, b2 *bool) bool {
	if b1 == nil && b2 == nil {
		return true
	}

	if b1 == nil || b2 == nil {
		return false
	}

	return *b1 == *b2
}
