#!/bin/bash

# Script to check the stats endpoint
echo "Fetching stats from http://localhost:8080/stats..."
echo ""

# Use jq to format the JSON output if available
if command -v jq &> /dev/null; then
    curl -s http://localhost:8080/stats | jq
else
    # If jq is not available, use plain curl
    curl http://localhost:8080/stats
fi

echo ""
echo "Note: The stats now include resource usage information for pods and nodes."
echo "- usage_report_pods: Shows resource requests, limits, and actual usage for each pod"
echo "- usage_report_nodes: Shows capacity, allocatable resources, and actual usage for each node"
