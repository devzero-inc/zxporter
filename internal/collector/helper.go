package collector

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
	return len(s) >= len(substr) && s[0:len(s)][0:len(substr)] == substr
}
