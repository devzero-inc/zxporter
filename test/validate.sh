#!/bin/bash

# Simple script to validate resource usage

# Default values
STATS_FILE=""
EXPECTED_FILE=""
TOLERANCE=10

# Parse command line arguments
while [[ $# -gt 0 ]]; do
  case $1 in
    --stats)
      STATS_FILE="$2"
      shift 2
      ;;
    --expected)
      EXPECTED_FILE="$2"
      shift 2
      ;;
    --tolerance)
      TOLERANCE="$2"
      shift 2
      ;;
    *)
      echo "Unknown option: $1"
      exit 1
      ;;
  esac
done

# Check if required arguments are provided
if [ -z "$STATS_FILE" ] || [ -z "$EXPECTED_FILE" ]; then
  echo "Usage: $0 --stats <stats_file> --expected <expected_file> [--tolerance <percentage>]"
  exit 1
fi

# Check if files exist
if [ ! -f "$STATS_FILE" ]; then
  echo "Stats file not found: $STATS_FILE"
  exit 1
fi

if [ ! -f "$EXPECTED_FILE" ]; then
  echo "Expected file not found: $EXPECTED_FILE"
  exit 1
fi

# Run the validator
echo "Running validator with tolerance: $TOLERANCE%"
go run ./test/validator/main.go --stats="$STATS_FILE" --expected="$EXPECTED_FILE" --tolerance="$TOLERANCE"
