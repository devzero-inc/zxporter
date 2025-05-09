#!/bin/bash

# Exit on error and pipefail to catch errors in pipes
set -eo pipefail

# Function to log messages
log() {
  echo "$(date +"%Y-%m-%d %H:%M:%S") - $1"
}

# Function to handle errors
handle_error() {
  log "ERROR: $1"
  # Continue execution (don't exit) since we need to run the main app regardless
}

log "Starting entrypoint script"

# Check if metrics-server is installed
log "Checking if metrics-server is installed..."
if ! kubectl get apiservice v1beta1.metrics.k8s.io &>/dev/null; then
  log "metrics-server not found, installing it now..."

  # Check if metrics-server.yaml exists
  if [ ! -f /metrics-server.yaml ]; then
    handle_error "metrics-server.yaml not found at /metrics-server.yaml"
  else
    # Apply the metrics-server yaml
    if kubectl apply -f /metrics-server.yaml; then
      log "metrics-server installed successfully"
    else
      handle_error "Failed to install metrics-server"
    fi

    # Wait for metrics-server to be ready
    log "Waiting for metrics-server to be ready..."
    ATTEMPTS=0
    MAX_ATTEMPTS=30

    while [ $ATTEMPTS -lt $MAX_ATTEMPTS ]; do
      if kubectl get apiservice v1beta1.metrics.k8s.io &>/dev/null; then
        log "metrics-server is now ready"
        break
      fi

      ATTEMPTS=$((ATTEMPTS+1))
      log "Waiting for metrics-server to be ready (attempt $ATTEMPTS/$MAX_ATTEMPTS)..."
      sleep 2
    done

    if [ $ATTEMPTS -eq $MAX_ATTEMPTS ]; then
      handle_error "metrics-server did not become ready in time"
    fi
  fi
else
  log "metrics-server is already installed"
fi

# Run the main application
log "Starting main application..."

# Execute the main command (assumes it's passed as arguments to this script)
exec "$@"
