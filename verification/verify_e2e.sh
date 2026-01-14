#!/bin/bash
set -e

CLUSTER_CONTEXT=${1:-kind-kind}
NAMESPACE=${2:-devzero-zxporter}

echo "Using cluster context: $CLUSTER_CONTEXT"

# 0. Ensure Kind cluster exists if using Kind
if [[ "$CLUSTER_CONTEXT" == "kind-zxporter-e2e" ]] || [[ "$CLUSTER_CONTEXT" == "kind-kind" ]]; then
    KIND_CLUSTER_NAME="${CLUSTER_CONTEXT#kind-}"
    if ! kind get clusters | grep -q "^${KIND_CLUSTER_NAME}$"; then
        echo "Cluster $KIND_CLUSTER_NAME does not exist. Creating..."
       # need `verification/kind-config.yaml`
        kind create cluster --name $KIND_CLUSTER_NAME --config verification/kind-config.yaml
    else
        echo "Cluster $KIND_CLUSTER_NAME already exists."
    fi
fi

kubectl config use-context $CLUSTER_CONTEXT

# 1. Build and push/load image
ARCH=$(go env GOARCH)
TAG="dev-$(date +%s)"
IMG="ttl.sh/zxporter-netmon:$TAG"

if [[ "$CLUSTER_CONTEXT" == "kind-zxporter-e2e" ]] || [[ "$CLUSTER_CONTEXT" == "kind-kind" ]]; then
    # Ensure nf_conntrack is loaded on the Kind node
    KIND_CLUSTER_NAME="${CLUSTER_CONTEXT#kind-}"
    echo "Ensuring nf_conntrack module is loaded on Kind nodes..."
    NODES=$(kind get nodes --name $KIND_CLUSTER_NAME)
    for NODE in $NODES; do
        docker exec $NODE modprobe nf_conntrack || true
    done
    
    echo "Building image for Kind..."
    # Kind needs the image loaded
    make docker-build-netmon IMG_NETMON=$IMG BUILD_ARGS="--load"
    echo "Loading image into Kind cluster: $KIND_CLUSTER_NAME..."
    kind load docker-image $IMG --name $KIND_CLUSTER_NAME
    PULL_POLICY="Never"
else
    echo "Building and pushing image for remote cluster..."
    # For EKS/GKE, we need to push to a registry accessible by the cluster
    # Assuming ttl.sh is accessible
    make docker-build-netmon IMG_NETMON=$IMG BUILD_ARGS="--push --platform linux/amd64,linux/arm64"
    # make docker-push-netmon IMG_NETMON=$IMG # Redundant if buildx --push is used
    PULL_POLICY="Always"
fi

# 2. Deploy zxporter-netmon
echo "Deploying zxporter-netmon..."

# Check if namespace exists, create if not
kubectl get namespace $NAMESPACE > /dev/null 2>&1 || kubectl create namespace $NAMESPACE

# 3.5. Deploy TestServer if enabled
if [[ "$ENABLE_CONTROL_PLANE_TEST" == "true" ]]; then
    echo "Deploying TestServer for Control Plane Verification..."
    
    # helper: Build testserver info
    TS_TAG="dev-ts-$(date +%s)"
    TS_IMG="ttl.sh/zxporter-testserver:$TS_TAG"
    
    # helper: Build and load/push
    echo "Building TestServer image..."
    # Reuse existing Dockerfile.testserver
    docker build -t $TS_IMG -f Dockerfile.testserver --build-arg GITHUB_TOKEN=$GITHUB_TOKEN .
    
    if [[ "$CLUSTER_CONTEXT" == "kind-zxporter-e2e" ]] || [[ "$CLUSTER_CONTEXT" == "kind-kind" ]]; then
        KIND_CLUSTER_NAME="${CLUSTER_CONTEXT#kind-}"
        kind load docker-image $TS_IMG --name $KIND_CLUSTER_NAME
    else
        docker push $TS_IMG
    fi
    
    # Deploy TestServer
    export TESTSERVER_IMG=$TS_IMG
    envsubst < test/testserver/kubernetes.yaml | sed "s/namespace: devzero-zxporter/namespace: $NAMESPACE/g" | kubectl apply -f -
    kubectl rollout status deployment/testserver -n $NAMESPACE --timeout=120s
    
    # Deploy zxporter (main chart) to provide the shared ConfigMap/CRDs
    echo "Deploying zxporter main chart (for shared ConfigMap)..."
    DAKR_URL="http://testserver.${NAMESPACE}.svc.cluster.local:50051"
    CLUSTER_TOKEN="test-token-001"
    # Determine Provider
    if [[ -n "$K8S_PROVIDER" ]]; then
        PROVIDER="$K8S_PROVIDER"
    elif [[ "$CLUSTER_CONTEXT" == kind-* ]]; then
        PROVIDER="other"
    elif [[ "$CLUSTER_CONTEXT" == arn:aws:eks:* ]]; then
        PROVIDER="aws"
    elif [[ "$CLUSTER_CONTEXT" == gke_* ]]; then
        PROVIDER="gcp"
    elif [[ "$CLUSTER_CONTEXT" == *aks* ]]; then
        PROVIDER="azure"
    else
        PROVIDER="other"
    fi
    
    helm upgrade --install zxporter ./helm-chart/zxporter \
        --namespace $NAMESPACE \
        --create-namespace \
        --set zxporter.dakrUrl="$DAKR_URL" \
        --set zxporter.clusterToken="$CLUSTER_TOKEN" \
        --set monitoring.enabled=false \
        --set highAvailability.enabled=false \
        --set zxporter.kubeContextName="$CLUSTER_CONTEXT" \
        --set zxporter.k8sProvider="$PROVIDER" \
        --set zxporter.logLevel="debug"
        
    echo "Updating zxporter-netmon configuration to point to TestServer..."
    # We re-run helm template with new values
    helm template zxporter-netmon helm-chart/zxporter-netmon \
        --namespace $NAMESPACE \
        --set image.repository=ttl.sh/zxporter-netmon \
        --set image.tag=$TAG \
        --set image.pullPolicy=$PULL_POLICY \
        --set image.pullPolicy=$PULL_POLICY \
        --set config.exportInterval="10s" \
        | kubectl apply -f -        
fi

# Use helm chart to generate manifest for simplicity or use the standalone manifest
# Let's use the Helm chart template command to generate a manifest with our image
helm template zxporter-netmon helm-chart/zxporter-netmon \
    --namespace $NAMESPACE \
    --set image.repository=ttl.sh/zxporter-netmon \
    --set image.tag=$TAG \
    --set image.pullPolicy=$PULL_POLICY \
    --set config.collectorMode="ebpf" \
    | kubectl apply -f -

echo "Restarting zxporter-netmon to pick up latest image..."
kubectl rollout restart daemonset/zxporter-netmon -n $NAMESPACE
kubectl rollout status daemonset/zxporter-netmon -n $NAMESPACE

# 3. Deploy traffic generator and server
echo "Deploying traffic workloads..."
kubectl delete -f verification/traffic-gen.yaml --ignore-not-found --cascade=foreground
kubectl delete -f verification/traffic-server.yaml --ignore-not-found --cascade=foreground
kubectl apply -f verification/traffic-gen.yaml
kubectl apply -f verification/traffic-server.yaml

# 4. Wait for pods
echo "Waiting for pods to be ready..."
kubectl wait --for=condition=ready pod -l app=zxporter-netmon -n $NAMESPACE --timeout=120s
kubectl wait --for=condition=ready pod -l app=traffic-gen -n default --timeout=120s
kubectl wait --for=condition=ready pod -l app=traffic-server -n default --timeout=120s

# 5. Verify Metrics
echo "Verifying metrics..."

# Force generic traffic to ensure we have something to capture immediately
TRAFFIC_PODS=$(kubectl get pod -l app=traffic-gen -n default --field-selector=status.phase=Running -o jsonpath="{.items[*].metadata.name}")
SERVER_SVC_IP=$(kubectl get svc traffic-server -n default -o jsonpath="{.spec.clusterIP}")
SERVER_POD_IPS=$(kubectl get pod -l app=traffic-server -n default --field-selector=status.phase=Running -o jsonpath="{.items[*].status.podIP}")

for TRAFFIC_POD in $TRAFFIC_PODS; do
    echo "Generating explicit traffic from $TRAFFIC_POD..."
    # Outbound
    DOMAINS=("google.com" "example.com" "devzero.io")
    for DOMAIN in "${DOMAINS[@]}"; do
        kubectl exec -n default $TRAFFIC_POD -- curl -s https://www.$DOMAIN > /dev/null || true
        kubectl exec -n default $TRAFFIC_POD -- nslookup $DOMAIN > /dev/null || true
    done

    # Pod-to-Pod
    echo "Generating pod-to-pod traffic to $SERVER_SVC_IP (traffic-server)..."
    kubectl exec -n default $TRAFFIC_POD -- curl -s --connect-timeout 2 http://traffic-server.default > /dev/null || true
done


# Use background process for port-forward
# Get all netmon pods
NETMON_PODS=$(kubectl get pods -l app=zxporter-netmon -n $NAMESPACE -o jsonpath='{.items[*].metadata.name}')
echo "Identified netmon pods: $NETMON_PODS"

# Loop until we get non-empty items
MAX_RETRIES=10
found=0

for i in $(seq 1 $MAX_RETRIES); do
    echo "Attempt $i/$MAX_RETRIES..."
    ITER_FOUND=0
    
    LOCAL_PORT=8081
    
    for POD_NAME in $NETMON_PODS; do
        echo "---------------------------------------------------"
        echo "Checking agent pod: $POD_NAME"
        
        # Determine Node Name for context
        NODE_NAME=$(kubectl get pod $POD_NAME -n $NAMESPACE -o jsonpath='{.spec.nodeName}')
        echo "Node: $NODE_NAME"

        echo "Fetching metrics from $POD_NAME via API proxy..."
        
        # Use kubectl proxy to fetch metrics (more reliable than port-forward in scripts)
        # Note: This works for both Kind and EKS as long as we have kubectl access
        RESPONSE=$(kubectl get --raw "/api/v1/namespaces/$NAMESPACE/pods/$POD_NAME:8081/proxy/metrics")
        
        # Save metrics for inspection
        echo "$RESPONSE" | jq . > verification/metrics-${NODE_NAME}.json
        
        # Check if items array is not empty
        if echo "$RESPONSE" | grep -q '"src_ip"'; then
            echo "Traffic detected on $NODE_NAME!"
            echo "$RESPONSE" | grep -o 'src_ip[^,]*' | head -n 5
            
            # Verify Pod-to-Pod flow specifically
            # We look for ANY traffic relevant to our test
            FOUND_TRAFFIC=0
            if echo "$RESPONSE" | grep -q "$SERVER_SVC_IP"; then
                FOUND_TRAFFIC=1
            fi
            
            for IP in $SERVER_POD_IPS; do
                if echo "$RESPONSE" | grep -q "$IP"; then
                    FOUND_TRAFFIC=1
                fi
            done
    
            if [ $FOUND_TRAFFIC -eq 1 ]; then
                echo " [OK] Pod-to-Pod traffic to $SERVER_SVC_IP / $SERVER_POD_IPS captured on $NODE_NAME."
                
                # Verify Pod Metadata
                if echo "$RESPONSE" | grep -q '"src_pod_name"'; then
                     echo " [OK] Pod Metadata (src_pod_name) enrichment detected."
                     ITER_FOUND=1
                else
                     echo " [FAIL] Pod Metadata missing on $NODE_NAME but traffic present!"
                fi
            else
                echo " [INFO] No relevant Pod-to-Pod traffic on this node yet."
            fi
        else
            echo "No flow traffic yet on $NODE_NAME."
        fi
        
        LOCAL_PORT=$((LOCAL_PORT+1))
    done
    
    if [ $ITER_FOUND -eq 1 ]; then
        found=1
        break
    fi
    
    echo "Waiting for traffic flows... "
    sleep 5
done

if [ $found -eq 0 ]; then
    echo "ERROR: No traffic flows detected after retries."
    exit 1
fi

echo ""
    
    if [[ "$ENABLE_CONTROL_PLANE_TEST" == "true" ]]; then
        echo "Validating Control Plane integration via TestServer..."
        
        # 6. Validate Data in TestServer
        echo "Querying TestServer stats..."
        
        # Use a temporary pod to curl the testserver (as it's cluster-internal)
        # Using alpine/curl as it might be smaller/more reliable, or just ensure IfNotPresent
        kubectl run curl-validator --image=curlimages/curl --image-pull-policy=IfNotPresent --restart=Never -n $NAMESPACE --command -- sleep 3600
        
        echo "Waiting for validator pod..."
        if ! kubectl wait --for=condition=Ready pod/curl-validator -n $NAMESPACE --timeout=120s; then
             echo " [ERROR] Validator pod failed to become ready."
             kubectl get pods -n $NAMESPACE
             kubectl describe pod curl-validator -n $NAMESPACE
             echo "TestServer Logs:"
             kubectl logs -l app=testserver -n $NAMESPACE
             exit 1
        fi
        
        echo "Executing curl (with retry)..."
        # Retry loop for stats availability (30s timeout)
        MAX_RETRIES=6
        RETRY_Count=0
        TOTAL_MSGS=0
        
        while [ $RETRY_Count -lt $MAX_RETRIES ]; do
            STATS_JSON=$(kubectl exec curl-validator -n $NAMESPACE -- curl -v -s http://testserver.${NAMESPACE}.svc.cluster.local:8080/stats)
            TOTAL_MSGS=$(echo $STATS_JSON | grep -o '"total_messages": [0-9]*' | awk '{print $2}')
            
            if [[ "$TOTAL_MSGS" -gt 0 ]]; then
                break
            fi
            
            echo " [INFO] TestServer has 0 messages. Waiting 5s for flush... ($((RETRY_Count+1))/$MAX_RETRIES)"
            sleep 5
            RETRY_Count=$((RETRY_Count+1))
        done
        
        kubectl delete pod curl-validator -n $NAMESPACE --ignore-not-found --cascade=foreground
        
        echo "Stats Response: $STATS_JSON"
        
        # Check Total Messages matches what we found
        if [[ "$TOTAL_MSGS" -gt 0 ]]; then
             echo " [OK] TestServer received $TOTAL_MSGS messages."
        else
             echo " [FAIL] TestServer received 0 messages after 30s."
             echo "DEBUG: Dumping TestServer Logs:"
             kubectl logs -l app=testserver -n $NAMESPACE --tail=50
             echo "DEBUG: Dumping Zxporter Netmon Logs:"
             kubectl logs -l app=zxporter-netmon -n $NAMESPACE --tail=50
             exit 1
        fi
        
        # Check Network Metrics
        # Simple string check for now, could use jq if installed
        if echo "$STATS_JSON" | grep -q '"network_metrics"'; then
             echo " [OK] Network Metrics received."
        else
             echo " [FAIL] No Network Metrics found in stats."
             echo "DEBUG: Dumping Stats JSON:"
             echo "$STATS_JSON"
             exit 1
        fi
        
        # Check DNS Lookups
        if echo "$STATS_JSON" | grep -q '"dns_lookups"'; then
             echo " [OK] DNS Lookups received."
        else
             echo " [WARN] No DNS Lookups found in stats (might be expected if no lookups performed)."
        fi
        
        # Check for enrichment in network metrics (src_pod_name)
        if echo "$STATS_JSON" | grep -q '"src_pod_name"'; then
             echo " [OK] Network Metrics contain Pod Metadata (enrichment working)."
        else
             echo " [FAIL] Network Metrics missing Pod Metadata."
             exit 1
        fi
    fi

echo ""
echo "E2E Verification Complete!"
