#!/bin/bash
set -eo pipefail
log() { echo "$(date +"%Y-%m-%d %H:%M:%S") - $1"; }
log "Starting entrypoint script"
log "Starting main application..."
exec "$@"
