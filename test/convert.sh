function convert_to_seconds() {
  local time_str=$1
  if [[ $time_str =~ ^([0-9]+)m([0-9]+(\.[0-9]+)?)s$ ]]; then
    mins=${BASH_REMATCH[1]}
    secs=${BASH_REMATCH[2]}
    # Using bc with scale to handle decimals
    echo "$(echo "scale=10; $mins * 60 + $secs" | bc)"
  elif [[ $time_str =~ ^([0-9]+)m$ ]]; then
    echo "$(echo "${BASH_REMATCH[1]} * 60" | bc)"
  elif [[ $time_str =~ ^([0-9]+(\.[0-9]+)?)s$ ]]; then
    echo "${BASH_REMATCH[1]}"
  else
    echo "Error: Invalid time format: $time_str" >&2
    return 1
  fi
}
