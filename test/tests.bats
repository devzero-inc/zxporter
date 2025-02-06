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
  
  # Extract the latest entry for test-app container based on lastUpdated timestamp
  latest_entry=$(echo "$output" | yq eval '.status.containers | map(select(.containerName == "test-app")) | sort_by(.lastUpdated) | .[-1]' -)
  
  # Extract CPU recommendation for the latest entry
  cpu_recommendation=$(echo "$latest_entry" | yq eval '.cpuRecommendation.adjustedRecommendation' -)
  
  # Extract memory recommendation for the latest entry
  memory_recommendation=$(echo "$latest_entry" | yq eval '.memoryRecommendation.adjustedRecommendation' -)
  
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
  if ! [ $(echo "$cpu_value >= 0.010" | bc -l) -eq 1 ] || ! [ $(echo "$cpu_value <= 0.035" | bc -l) -eq 1 ]; then
    echo "ERROR: CPU recommendation $cpu_value is outside the expected range [0.010, 0.035]" >&2
    return 1
  fi
  
  # Check if memory recommendation is within range
  echo "Memory recommendation: $memory_value" >&2
  if ! [ $(echo "$memory_value >= 50.00" | bc -l) -eq 1 ] || ! [ $(echo "$memory_value <= 85.00" | bc -l) -eq 1 ]; then
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

function convert_to_seconds() {
  local time_str=$1
  if [[ $time_str =~ ^([0-9]+)m([0-9]+(\.[0-9]+)?)s$ ]]; then
    mins=${BASH_REMATCH[1]}
    secs=${BASH_REMATCH[2]}
    echo "$(echo "$mins * 60 + $secs" | bc | awk '{print int($1+0.5)}')"
  elif [[ $time_str =~ ^([0-9]+)m$ ]]; then
    echo "$((BASH_REMATCH[1] * 60))"
  elif [[ $time_str =~ ^([0-9]+(\.[0-9]+)?)s$ ]]; then
    echo "$(echo "${BASH_REMATCH[1]}" | awk '{print int($1+0.5)}')"
  else
    echo "Error: Invalid time format: $time_str" >&2
    return 1
  fi
}

@test "test_frequency_changes" {
  max_attempts=90  # 15 minutes with 10-second intervals
  attempt=1
  
  # Initial check for 30s frequency
  echo "Checking initial frequency (should be 30s)..." >&2
  
  initial_frequency_found=false
  for i in {1..12}; do  # Try for 2 minutes
    run kubectl logs deployment.apps/resource-adjustment-operator-controller-manager -n resource-adjustment-operator-system
    [ "$status" -eq 0 ]
    
    latest_freq=$(echo "$output" | grep "frequency-test-app-1" | grep "controlSignal" | grep "newFrequency" | tail -n 1 | grep -o 'newFrequency": "[^"]*' | cut -d'"' -f3)
    
    if [ "$latest_freq" = "30s" ]; then
      initial_frequency_found=true
      echo "Found initial frequency of 30s" >&2
      break
    fi
    sleep 10
  done
  
  if [ "$initial_frequency_found" = false ]; then
    echo "ERROR: Initial frequency of 30s not found" >&2
    return 1
  fi
  
  # Monitor frequency changes
  found_significant_increase=false
  found_decrease_to_30s=false
  last_freq="30s"
  max_freq_seen=30  # in seconds
  
  while [ $attempt -le $max_attempts ]; do
    echo "Attempt $attempt of $max_attempts checking frequency changes..." >&2
    
    run kubectl logs deployment.apps/resource-adjustment-operator-controller-manager -n resource-adjustment-operator-system
    [ "$status" -eq 0 ]
    
    latest_freq=$(echo "$output" | grep "frequency-test-app-1" | grep "controlSignal" | grep "newFrequency" | tail -n 1 | grep -o 'newFrequency": "[^"]*' | cut -d'"' -f3)
    
    if [ -n "$latest_freq" ]; then
      echo "Current frequency: $latest_freq (Previous: $last_freq)" >&2
      
      latest_seconds=$(convert_to_seconds "$latest_freq")
      
      if [ "$latest_seconds" -gt "$max_freq_seen" ]; then
        max_freq_seen=$latest_seconds
      fi
      
      # Check for significant increase (more than 10 minutes)
      if [ "$latest_seconds" -gt 600 ]; then  # 10 minutes in seconds
        echo "Detected significant frequency increase to $latest_freq" >&2
        found_significant_increase=true
      fi
      
      # Check for decrease back to 30s
      if [ "$found_significant_increase" = true ] && [ "$latest_freq" = "30s" ]; then
        echo "Detected frequency decrease back to 30s" >&2
        found_decrease_to_30s=true
        break
      fi
      
      last_freq=$latest_freq
    fi
    
    sleep 10
    ((attempt++))
  done
  
  if [ "$found_significant_increase" = false ]; then
    echo "ERROR: Frequency never showed significant increase (max seen: ${max_freq_seen}s)" >&2
    return 1
  fi
  
  if [ "$found_decrease_to_30s" = false ]; then
    echo "ERROR: Frequency never decreased back to 30s" >&2
    return 1
  fi
  
  echo "Maximum frequency reached: ${max_freq_seen}s (${max_freq_seen/60}m${max_freq_seen%60}s)" >&2
  echo "All frequency transitions verified successfully!" >&2
}

# Function to fetch and validate recommendations
function check_recommendations() {
  local cpu_min=$1
  local cpu_max=$2
  local mem_min=$3
  local mem_max=$4
  local phase_name=$5
  
  run kubectl get resourceadjustmentpolicy resourceadjustmentpolicy-sample -o yaml
  [ "$status" -eq 0 ]
  
  # Extract the latest entry for container based on lastUpdated timestamp
  latest_entry=$(echo "$output" | yq eval ".status.containers | map(select(.containerName == \"$CONTAINER_NAME\")) | sort_by(.lastUpdated) | .[-1]" -)
  
  # Extract recommendations
  cpu_recommendation=$(echo "$latest_entry" | yq eval '.cpuRecommendation.adjustedRecommendation' -)
  memory_recommendation=$(echo "$latest_entry" | yq eval '.memoryRecommendation.adjustedRecommendation' -)
  
  # Convert to numeric values
  cpu_value=$(echo "$cpu_recommendation" | awk '{print $1+0}')
  memory_value=$(echo "$memory_recommendation" | awk '{print $1+0}')
  
  echo "Phase: $phase_name - CPU: $cpu_value (Expected: $cpu_min-$cpu_max), Memory: $memory_value (Expected: $mem_min-$mem_max)" >&2
  
  # Validate CPU
  if ! [ $(echo "$cpu_value >= $cpu_min" | bc -l) -eq 1 ] || ! [ $(echo "$cpu_value <= $cpu_max" | bc -l) -eq 1 ]; then
    echo "ERROR: CPU recommendation $cpu_value is outside expected range [$cpu_min, $cpu_max] in phase $phase_name" >&2
    return 1
  fi
  
  # Validate Memory
  if ! [ $(echo "$memory_value >= $mem_min" | bc -l) -eq 1 ] || ! [ $(echo "$memory_value <= $mem_max" | bc -l) -eq 1 ]; then
    echo "ERROR: Memory recommendation $memory_value is outside expected range [$mem_min, $mem_max] in phase $phase_name" >&2
    return 1
  fi
}

# Function to check frequency
function check_frequency() {
  local expected_freq=$1
  local phase_name=$2
  local greater_than=${3:-false}
  
  run kubectl logs deployment.apps/resource-adjustment-operator-controller-manager -n resource-adjustment-operator-system
  [ "$status" -eq 0 ]
  
  latest_freq=$(echo "$output" | grep "$CONTAINER_NAME" | grep "controlSignal" | grep "newFrequency" | tail -n 1 | grep -o 'newFrequency": "[^"]*' | cut -d'"' -f3)  

  if [ -z "$latest_freq" ]; then
    echo "ERROR: No frequency found for $CONTAINER_NAME in phase $phase_name" >&2
    return 1
  fi
  
  latest_seconds=$(convert_to_seconds "$latest_freq")
  expected_seconds=$(convert_to_seconds "$expected_freq")
  
  echo "Phase: $phase_name - Current frequency: $latest_freq (Expected: $expected_freq)" >&2
  
  if [ "$greater_than" = "true" ]; then
    if ! [ "$latest_seconds" -gt "$expected_seconds" ]; then
      echo "ERROR: Frequency $latest_freq is not greater than $expected_freq in phase $phase_name" >&2
      return 1
    fi
  else
    if [ "$latest_freq" != "$expected_freq" ]; then
      echo "ERROR: Frequency $latest_freq does not match expected $expected_freq in phase $phase_name" >&2
      return 1
    fi
  fi
}

function monitor_frequency_with_condition() {
  local expected_freq=$1
  local phase_name=$2
  local duration=180  # 3 minutes
  local frequency_found=false
  local start_time=$SECONDS
  local end_time=$((SECONDS + duration))
  
  while [ $SECONDS -lt $end_time ]; do
    run kubectl logs deployment.apps/resource-adjustment-operator-controller-manager -n resource-adjustment-operator-system
    [ "$status" -eq 0 ]
    
    latest_freq=$(echo "$output" | grep "$CONTAINER_NAME" | grep "controlSignal" | grep "newFrequency" | tail -n 1 | grep -o 'newFrequency": "[^"]*' | cut -d'"' -f3)
    
    if [ -z "$latest_freq" ]; then
      echo "No frequency log found in this iteration" >&2
      sleep 30
      continue
    fi
    
    latest_seconds=$(convert_to_seconds "$latest_freq")
    expected_seconds=$(convert_to_seconds "$expected_freq")
    
    echo "Monitoring $phase_name - Current frequency: $latest_freq (Expected > $expected_freq)" >&2
    
    if [ "$latest_seconds" -gt "$expected_seconds" ]; then
      echo "Frequency requirement met in $phase_name: $latest_freq > $expected_freq" >&2
      frequency_found=true
      break
    fi
    
    sleep 30
  done
  
  # If frequency not found within 3 minutes, calculate remaining wait time
  if [ "$frequency_found" = false ]; then
    echo "ERROR: Frequency never exceeded $expected_freq in $phase_name" >&2
    return 1
  else
    # If found before the end, wait remaining time
    remaining_time=$((end_time - SECONDS))
    if [ "$remaining_time" -gt 0 ]; then
      echo "Frequency found early. Waiting remaining $remaining_time seconds..." >&2
      sleep "$remaining_time"
    fi
  fi
}

@test "test_container_burst_phases" {
  # Ensure CONTAINER_NAME environment variable is set
  if [ -z "$CONTAINER_NAME" ]; then
    echo "ERROR: CONTAINER_NAME environment variable must be set" >&2
    return 1
  fi
  
  echo "Starting burst test for container: $CONTAINER_NAME" >&2
  
  # Initial phase
  echo "Initial Phase: Checking baseline metrics..." >&2
  check_recommendations 0.06 0.13 100 300 "Initial"
  check_frequency "30s" "Initial"
  
  # Burst 1
  echo "Burst 1: Waiting 6 minutes..." >&2
  sleep 360
  check_recommendations 0.4 0.55 600 1100 "Burst-1"
  sleep 120
  check_frequency "30s" "Burst-1" "true"
  
  # Monitor frequency for 3 minutes
  echo "Burst 1: Monitoring frequency for 3 minutes..." >&2
  monitor_frequency_with_condition "10m" "Burst-1-Monitor"
  
  # Burst 2
  echo "Burst 2: Waiting 10 minutes..." >&2
  sleep 600
  check_recommendations 0.1 0.15 1300 1900 "Burst-2"
  sleep 60
  check_frequency "30s" "Burst-2"
  
  # Burst 3
  echo "Burst 3: Waiting 10 minutes..." >&2
  sleep 600
  check_recommendations 0.4 0.55 600 1100 "Burst-3"
  sleep 10
  
  # Monitor frequency for 3 minutes
  echo "Burst 3: Monitoring frequency for 3 minutes..." >&2
  monitor_frequency_with_condition "10m" "Burst-3-Monitor"
  
  # Burst 4
  echo "Burst 4: Waiting 8 minutes..." >&2
  sleep 480
  check_recommendations 1.1 1.35 600 1100 "Burst-4"
  check_frequency "30s" "Burst-4"
  
  echo "All burst phases completed successfully!" >&2
  return 1
}
