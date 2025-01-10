#!/bin/bash

wait_for_deployment() {
    local deployment_name="$1"
    local namespace_name="$2"
    
    if [[ -z "$deployment_name" ]] || [[ -z "$namespace_name" ]]; then
        echo "Usage: $0 <deployment_name> <namespace_name>"
        exit 1
    fi

    timeout=60 # 5 minutes (60 * 5 sec)
    i=1
    
    echo "Checking if the ${deployment_name} deployment is ready in namespace ${namespace_name}"
    
    until kubectl -n "${namespace_name}" get deployment "${deployment_name}" -o jsonpath='{.status.conditions[?(@.status=="True")].type}' | grep "Available" 2>/dev/null; do
        ((i++))
        if [[ ${i} -gt ${timeout} ]]; then
            echo "The ${deployment_name} deployment in namespace ${namespace_name} has not become ready before the timeout period"
            echo "Fetching deployment status and describing pods for debugging:"
            kubectl -n "${namespace_name}" get deployment "${deployment_name}"
            kubectl -n "${namespace_name}" describe deployment "${deployment_name}"
            kubectl -n "${namespace_name}" get pods
            kubectl -n "${namespace_name}" describe pods
            
            echo "Fetching detailed logs from the pods for further debugging:"
            for pod in $(kubectl -n "${namespace_name}" get pods -o jsonpath='{.items[*].metadata.name}'); do
                echo "Logs for pod ${pod}:"
                kubectl -n "${namespace_name}" logs "${pod}"
            done
            exit 1
        fi
        echo "Waiting for ${deployment_name} deployment to report a ready status in namespace ${namespace_name} (Attempt ${i}/${timeout})"
        sleep 5
    done
    
    echo "The ${deployment_name} deployment in namespace ${namespace_name} is ready"
}

wait_for_deployment "$1" "$2"
