#!/usr/bin/env bats

function log_and_run() {
  echo "Running: $*" >&2
  "$@"
  status=$?
  echo "Status: $status" >&2
  if [ "$status" -ne 0 ]; then
    echo "Command failed with status $status: $*" >&2
    echo "Output:" >&2
    echo "$output" >&2
  fi
  return $status
}

@test "test_resource_adjustment_recommendations" {
  max_attempts=5
  attempt=1
  container_found=false
  
  while [ $attempt -le $max_attempts ]; do
    echo "Attempt $attempt of $max_attempts to find test-app container..." >&2
    
    # Get the ResourceAdjustmentPolicy YAML
    run kubectl get resourceadjustmentpolicy resourceadjustmentpolicy-sample -o yaml
    [ "$status" -eq 0 ]
    
    # Check if test-app container exists in status
    if echo "$output" | yq eval '.status.containers[] | select(.containerName == "test-app")' - | grep -q "test-app"; then
      echo "Found test-app container in status" >&2
      container_found=true
      break
    else
      echo "test-app container not found in status, waiting 20 seconds..." >&2
      sleep 20
      ((attempt++))
    fi
  done
  
  # Fail if container was not found after all attempts
  if [ "$container_found" = false ]; then
    echo "ERROR: test-app container not found in status after $max_attempts attempts" >&2
    echo "Last received YAML:" >&2
    echo "$output" >&2
    return 1
  fi
  
  # Extract CPU recommendation for test-app container
  cpu_recommendation=$(echo "$output" | yq eval '.status.containers[] | select(.containerName == "test-app") | .cpuRecommendation.adjustedRecommendation' -)
  
  # Extract memory recommendation for test-app container
  memory_recommendation=$(echo "$output" | yq eval '.status.containers[] | select(.containerName == "test-app") | .memoryRecommendation.adjustedRecommendation' -)
  
  # Check if recommendations are empty or null
  if [ -z "$cpu_recommendation" ] || [ "$cpu_recommendation" = "null" ]; then
    echo "ERROR: CPU recommendation is empty or null for test-app container" >&2
    echo "Last received YAML:" >&2
    echo "$output" >&2
    return 1
  fi
  
  if [ -z "$memory_recommendation" ] || [ "$memory_recommendation" = "null" ]; then
    echo "ERROR: Memory recommendation is empty or null for test-app container" >&2
    echo "Last received YAML:" >&2
    echo "$output" >&2
    return 1
  fi
  
  # Convert recommendations to floating point numbers for comparison
  cpu_value=$(echo "$cpu_recommendation" | awk '{print $1+0}')
  memory_value=$(echo "$memory_recommendation" | awk '{print $1+0}')
  
  # Check if CPU recommendation is within range
  echo "CPU recommendation: $cpu_value" >&2
  if ! [ $(echo "$cpu_value >= 0.015" | bc -l) -eq 1 ] || ! [ $(echo "$cpu_value <= 0.035" | bc -l) -eq 1 ]; then
    echo "ERROR: CPU recommendation $cpu_value is outside the expected range [0.015, 0.035]" >&2
    return 1
  fi
  
  # Check if memory recommendation is within range
  echo "Memory recommendation: $memory_value" >&2
  if ! [ $(echo "$memory_value >= 63.00" | bc -l) -eq 1 ] || ! [ $(echo "$memory_value <= 85.00" | bc -l) -eq 1 ]; then
    echo "ERROR: Memory recommendation $memory_value is outside the expected range [63.00, 85.00]" >&2
    return 1
  fi
  
  echo "All checks passed successfully!" >&2
}
