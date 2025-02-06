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
