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

@test "test_increasing_load_recommendations" {
  iterations=3
  wait_time=90
  min_increase_percent=10
  container_name="inc-load"
  
  # Arrays to store historical recommendations
  declare -a cpu_history
  declare -a memory_history
  
  for ((i=1; i<=iterations; i++)); do
    echo "Iteration $i of $iterations" >&2
    
    # Get the ResourceAdjustmentPolicy YAML
    run kubectl get resourceadjustmentpolicy resourceadjustmentpolicy-sample -o yaml
    [ "$status" -eq 0 ]
    
    # Check if inc-load container exists in status
    if ! echo "$output" | yq eval ".status.containers[] | select(.containerName == \"$container_name\")" - | grep -q "$container_name"; then
      echo "ERROR: $container_name container not found in status" >&2
      echo "YAML content:" >&2
      echo "$output" >&2
      return 1
    fi
    
    # Extract current recommendations
    cpu_recommendation=$(echo "$output" | yq eval ".status.containers[] | select(.containerName == \"$container_name\") | .cpuRecommendation.adjustedRecommendation" -)
    memory_recommendation=$(echo "$output" | yq eval ".status.containers[] | select(.containerName == \"$container_name\") | .memoryRecommendation.adjustedRecommendation" -)
    
    # Validate recommendations are not empty
    if [ -z "$cpu_recommendation" ] || [ "$cpu_recommendation" = "null" ]; then
      echo "ERROR: CPU recommendation is empty or null for $container_name container" >&2
      return 1
    fi
    
    if [ -z "$memory_recommendation" ] || [ "$memory_recommendation" = "null" ]; then
      echo "ERROR: Memory recommendation is empty or null for $container_name container" >&2
      return 1
    fi
    
    # Convert to numeric values
    cpu_value=$(echo "$cpu_recommendation" | awk '{print $1+0}')
    memory_value=$(echo "$memory_recommendation" | awk '{print $1+0}')
    
    echo "Current CPU recommendation: $cpu_value" >&2
    echo "Current Memory recommendation: $memory_value" >&2
    
    # Store current values in history
    cpu_history[$i]=$cpu_value
    memory_history[$i]=$memory_value
    
    # Check for increase if not first iteration
    if [ $i -gt 1 ]; then
      prev_cpu=${cpu_history[$((i-1))]}
      prev_memory=${memory_history[$((i-1))]}
      
      # Calculate percentage increases
      cpu_increase=$(echo "scale=2; ($cpu_value - $prev_cpu) / $prev_cpu * 100" | bc)
      memory_increase=$(echo "scale=2; ($memory_value - $prev_memory) / $prev_memory * 100" | bc)
      
      echo "CPU increase: $cpu_increase%" >&2
      echo "Memory increase: $memory_increase%" >&2
      
      # Check if either CPU or memory increased by at least min_increase_percent
      if [ $(echo "$cpu_increase < $min_increase_percent" | bc -l) -eq 1 ] && [ $(echo "$memory_increase < $min_increase_percent" | bc -l) -eq 1 ]; then
        echo "ERROR: Neither CPU nor memory recommendations increased by at least $min_increase_percent%" >&2
        echo "CPU increase: $cpu_increase%" >&2
        echo "Memory increase: $memory_increase%" >&2
        return 1
      fi
    fi
    
    # Wait before next iteration if not the last one
    if [ $i -lt $iterations ]; then
      echo "Waiting $wait_time seconds before next check..." >&2
      sleep $wait_time
    fi
  done
  
  echo "All checks passed successfully!" >&2
  echo "Final recommendations:" >&2
  echo "CPU history: ${cpu_history[*]}" >&2
  echo "Memory history: ${memory_history[*]}" >&2
}

@test "test_decreasing_load_recommendations" {
  iterations=3
  wait_time=90
  min_decrease_percent=30
  container_name="dec-load"
  
  # Arrays to store historical recommendations
  declare -a cpu_history
  declare -a memory_history
  
  for ((i=1; i<=iterations; i++)); do
    echo "Iteration $i of $iterations" >&2
    
    # Get the ResourceAdjustmentPolicy YAML
    run kubectl get resourceadjustmentpolicy resourceadjustmentpolicy-sample -o yaml
    [ "$status" -eq 0 ]
    
    # Check if dec-load container exists in status
    if ! echo "$output" | yq eval ".status.containers[] | select(.containerName == \"$container_name\")" - | grep -q "$container_name"; then
      echo "ERROR: $container_name container not found in status" >&2
      echo "YAML content:" >&2
      echo "$output" >&2
      return 1
    fi
    
    # Extract current recommendations
    cpu_recommendation=$(echo "$output" | yq eval ".status.containers[] | select(.containerName == \"$container_name\") | .cpuRecommendation.adjustedRecommendation" -)
    memory_recommendation=$(echo "$output" | yq eval ".status.containers[] | select(.containerName == \"$container_name\") | .memoryRecommendation.adjustedRecommendation" -)
    
    # Validate recommendations are not empty
    if [ -z "$cpu_recommendation" ] || [ "$cpu_recommendation" = "null" ]; then
      echo "ERROR: CPU recommendation is empty or null for $container_name container" >&2
      return 1
    fi
    
    if [ -z "$memory_recommendation" ] || [ "$memory_recommendation" = "null" ]; then
      echo "ERROR: Memory recommendation is empty or null for $container_name container" >&2
      return 1
    fi
    
    # Convert to numeric values
    cpu_value=$(echo "$cpu_recommendation" | awk '{print $1+0}')
    memory_value=$(echo "$memory_recommendation" | awk '{print $1+0}')
    
    echo "Current CPU recommendation: $cpu_value" >&2
    echo "Current Memory recommendation: $memory_value" >&2
    
    # Store current values in history
    cpu_history[$i]=$cpu_value
    memory_history[$i]=$memory_value
    
    # Check for decrease if not first iteration
    if [ $i -gt 1 ]; then
      prev_cpu=${cpu_history[$((i-1))]}
      prev_memory=${memory_history[$((i-1))]}
      
      # Calculate percentage decreases
      cpu_decrease=$(echo "scale=2; ($prev_cpu - $cpu_value) / $prev_cpu * 100" | bc)
      memory_decrease=$(echo "scale=2; ($prev_memory - $memory_value) / $prev_memory * 100" | bc)
      
      echo "CPU decrease: $cpu_decrease%" >&2
      echo "Memory decrease: $memory_decrease%" >&2
      
      # Check if either CPU or memory decreased by at least min_decrease_percent
      if [ $(echo "$cpu_decrease < $min_decrease_percent" | bc -l) -eq 1 ] && [ $(echo "$memory_decrease < $min_decrease_percent" | bc -l) -eq 1 ]; then
        echo "ERROR: Neither CPU nor memory recommendations decreased by at least $min_decrease_percent%" >&2
        echo "CPU decrease: $cpu_decrease%" >&2
        echo "Memory decrease: $memory_decrease%" >&2
        echo "Previous CPU: $prev_cpu, Current CPU: $cpu_value" >&2
        echo "Previous Memory: $prev_memory, Current Memory: $memory_value" >&2
        return 1
      fi
      
      # Additional check to ensure we're not going too low
      if [ $(echo "$cpu_value < 0.05" | bc -l) -eq 1 ]; then
        echo "WARNING: CPU recommendation ($cpu_value) has gone below minimum threshold of 0.05" >&2
      fi
      
      if [ $(echo "$memory_value < 50" | bc -l) -eq 1 ]; then
        echo "WARNING: Memory recommendation ($memory_value) has gone below minimum threshold of 50Mi" >&2
      fi
    fi
    
    # Wait before next iteration if not the last one
    if [ $i -lt $iterations ]; then
      echo "Waiting $wait_time seconds before next check..." >&2
      sleep $wait_time
    fi
  done
  
  echo "All checks passed successfully!" >&2
  echo "Resource recommendation history:" >&2
  echo "CPU history (cores): ${cpu_history[*]}" >&2
  echo "Memory history (Mi): ${memory_history[*]}" >&2
  
  # Final check to ensure we've decreased significantly from start to finish
  if [ ${#cpu_history[@]} -ge 2 ]; then
    total_cpu_decrease=$(echo "scale=2; (${cpu_history[1]} - ${cpu_history[-1]}) / ${cpu_history[1]} * 100" | bc)
    total_memory_decrease=$(echo "scale=2; (${memory_history[1]} - ${memory_history[-1]}) / ${memory_history[1]} * 100" | bc)
    echo "Total CPU decrease from start to finish: $total_cpu_decrease%" >&2
    echo "Total Memory decrease from start to finish: $total_memory_decrease%" >&2
  fi
}
