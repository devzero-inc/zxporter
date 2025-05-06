package provider

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseAKSResourceGroupName_Valid(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected AKSMetadata
	}{
		{
			name:  "Standard case",
			input: "MC_dev-test_devzero_eastus",
			expected: AKSMetadata{
				ResourceGroup: "dev-test",
				ClusterName:   "devzero",
				Region:        "eastus",
			},
		},
		{
			name:  "Hyphenated parts",
			input: "MC_devzero-rg_devzero-cluster_westus2",
			expected: AKSMetadata{
				ResourceGroup: "devzero-rg",
				ClusterName:   "devzero-cluster",
				Region:        "westus2",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			meta, err := parseAKSResourceGroupName(tt.input)
			require.NoError(t, err)
			require.Equal(t, &tt.expected, meta)
		})
	}
}

func Test_parseAKSResourceGroupName_Invalid(t *testing.T) {
	tests := []string{
		"MC_onlytwo_parts",
		"MC_foo_bar", // missing region
		"WRONGPREFIX_rg_cluster_region",
		"MC_rg_cluster", // missing region
		"",              // empty
	}

	for _, input := range tests {
		t.Run(input, func(t *testing.T) {
			meta, err := parseAKSResourceGroupName(input)
			require.Nil(t, meta)
			require.Error(t, err)
		})
	}
}

func Test_getClusterNameFromProviderID(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "happy path",
			input:    "azure:///subscriptions/a32b188c-43fd-4e28-8f67-c95649ab3119/resourceGroups/mc_dev-test_devzero_eastus/providers/Microsoft.Compute/virtualMachineScaleSets/aks-systemnp-35145560-vmss/virtualMachines/0",
			expected: "devzero",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clusterName, err := getClusterNameFromProviderID(tt.input)
			require.Nil(t, err)
			require.Equal(t, tt.expected, clusterName)
		})
	}
}
