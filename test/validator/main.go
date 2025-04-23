package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"strconv"
	"strings"

	"github.com/devzero-inc/zxporter/test/stats"
)

// parseResourceValue parses a resource value string (like "500m" or "64Mi") to a numeric value
func parseResourceValue(value string) (float64, error) {
	// Handle CPU millicores
	if strings.HasSuffix(value, "m") {
		millicores, err := strconv.ParseFloat(strings.TrimSuffix(value, "m"), 64)
		if err != nil {
			return 0, err
		}
		return millicores, nil
	}

	// Handle memory units
	memoryUnits := map[string]float64{
		"Ki": 1024,
		"Mi": 1024 * 1024,
		"Gi": 1024 * 1024 * 1024,
		"Ti": 1024 * 1024 * 1024 * 1024,
		"K":  1000,
		"M":  1000 * 1000,
		"G":  1000 * 1000 * 1000,
		"T":  1000 * 1000 * 1000 * 1000,
	}

	for suffix, multiplier := range memoryUnits {
		if strings.HasSuffix(value, suffix) {
			numValue, err := strconv.ParseFloat(strings.TrimSuffix(value, suffix), 64)
			if err != nil {
				return 0, err
			}
			return numValue * multiplier, nil
		}
	}

	// No unit suffix, try to parse as a plain number
	return strconv.ParseFloat(value, 64)
}

// compareResourceValues compares two resource values with a tolerance
func compareResourceValues(actual, expected string, tolerance float64, isStrict bool) (bool, string, float64) {
	// For strict comparison (requests and limits), we compare the strings directly
	if isStrict {
		if actual == expected {
			return true, "", 0
		}
		return false, fmt.Sprintf("expected %s, got %s", expected, actual), 0
	}

	// For non-strict comparison (usage), we parse the values and apply tolerance
	actualValue, err := parseResourceValue(actual)
	if err != nil {
		return false, fmt.Sprintf("failed to parse actual value %s: %v", actual, err), 0
	}

	expectedValue, err := parseResourceValue(expected)
	if err != nil {
		return false, fmt.Sprintf("failed to parse expected value %s: %v", expected, err), 0
	}

	// Calculate the difference percentage
	var diffPercentage float64
	if expectedValue != 0 {
		diffPercentage = (actualValue - expectedValue) / expectedValue * 100
		if diffPercentage < 0 {
			diffPercentage = -diffPercentage
		}
	} else if actualValue != 0 {
		diffPercentage = 100 // If expected is 0 but actual is not, that's a 100% difference
	} else {
		diffPercentage = 0 // Both are 0, no difference
	}

	if diffPercentage <= tolerance {
		return true, "", diffPercentage
	}

	return false, fmt.Sprintf("expected %s, got %s (diff: %.2f%%)", expected, actual, diffPercentage), diffPercentage
}

// validatePods validates the actual pod usage against expected pod usage
func validatePods(actual, expected map[string]stats.PodResourceUsage, tolerance float64) (bool, []string) {
	var errors []string
	valid := true

	// Check if all expected pods are present in actual
	for podName, expectedPod := range expected {
		actualPod, exists := actual[podName]
		if !exists {
			valid = false
			errors = append(errors, fmt.Sprintf("Pod %s not found in actual data", podName))
			continue
		}

		// Validate requests (strict)
		for resourceName, expectedValue := range expectedPod.Requests {
			actualValue, exists := actualPod.Requests[resourceName]
			if !exists {
				valid = false
				errors = append(errors, fmt.Sprintf("Pod %s: request for %s not found", podName, resourceName))
				continue
			}

			match, errMsg, _ := compareResourceValues(actualValue, expectedValue, 0, true)
			if !match {
				valid = false
				errors = append(errors, fmt.Sprintf("Pod %s: request for %s %s", podName, resourceName, errMsg))
			}
		}

		// Validate limits (strict)
		for resourceName, expectedValue := range expectedPod.Limits {
			actualValue, exists := actualPod.Limits[resourceName]
			if !exists {
				valid = false
				errors = append(errors, fmt.Sprintf("Pod %s: limit for %s not found", podName, resourceName))
				continue
			}

			match, errMsg, _ := compareResourceValues(actualValue, expectedValue, 0, true)
			if !match {
				valid = false
				errors = append(errors, fmt.Sprintf("Pod %s: limit for %s %s", podName, resourceName, errMsg))
			}
		}

		// Validate containers (with tolerance)
		for containerName, expectedContainer := range expectedPod.Containers {
			actualContainer, exists := actualPod.Containers[containerName]
			if !exists {
				valid = false
				errors = append(errors, fmt.Sprintf("Pod %s: container %s not found", podName, containerName))
				continue
			}

			for metricName, expectedValue := range expectedContainer {
				actualValue, exists := actualContainer[metricName]
				if !exists {
					valid = false
					errors = append(errors, fmt.Sprintf("Pod %s: container %s: metric %s not found", podName, containerName, metricName))
					continue
				}

				match, errMsg, diffPercentage := compareResourceValues(actualValue, expectedValue, tolerance, false)
				if !match {
					valid = false
					errors = append(errors, fmt.Sprintf("Pod %s: container %s: metric %s %s", podName, containerName, metricName, errMsg))
				} else {
					fmt.Printf("✅ Pod %s: container %s: metric %s within tolerance (diff: %.2f%%)\n", podName, containerName, metricName, diffPercentage)
				}
			}
		}
	}

	return valid, errors
}

func main() {
	// Define command-line flags
	statsFile := flag.String("stats", "", "Path to the stats JSON file")
	expectedFile := flag.String("expected", "", "Path to the expected pods JSON file")
	tolerance := flag.Float64("tolerance", 10.0, "Tolerance percentage for container usage validation")

	// Parse command-line flags
	flag.Parse()

	// Check if required flags are provided
	if *statsFile == "" || *expectedFile == "" {
		fmt.Println("Error: Both --stats and --expected flags are required")
		flag.Usage()
		os.Exit(1)
	}

	// Read stats file
	statsData, err := ioutil.ReadFile(*statsFile)
	if err != nil {
		fmt.Printf("Error reading stats file: %v\n", err)
		os.Exit(1)
	}

	// Read expected file
	expectedData, err := ioutil.ReadFile(*expectedFile)
	if err != nil {
		fmt.Printf("Error reading expected file: %v\n", err)
		os.Exit(1)
	}

	// Parse stats JSON
	var statsObj stats.Stats
	if err := json.Unmarshal(statsData, &statsObj); err != nil {
		fmt.Printf("Error parsing stats JSON: %v\n", err)
		os.Exit(1)
	}

	// Parse expected JSON
	var expectedObj stats.ExpectedPods
	if err := json.Unmarshal(expectedData, &expectedObj); err != nil {
		fmt.Printf("Error parsing expected JSON: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Validating pod resource usage with tolerance: %.2f%%\n", *tolerance)

	// Validate pods
	valid, errors := validatePods(statsObj.UsageReportPods, expectedObj.UsageReportPods, *tolerance)
	if !valid {
		fmt.Println("\n❌ Validation failed with the following errors:")
		for _, err := range errors {
			fmt.Printf("  - %s\n", err)
		}
		os.Exit(1)
	}

	fmt.Println("\n✅ Validation successful!")
}
