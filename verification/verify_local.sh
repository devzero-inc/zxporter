#!/bin/bash
set -e

# build zxporter-netmon for linux (matching host arch for Lima)
ARCH=$(go env GOARCH)
echo "Building zxporter-netmon for Linux/$ARCH..."
GOOS=linux GOARCH=$ARCH go build -o bin/zxporter-netmon-linux ./cmd/zxporter-netmon/main.go

echo "Build complete: bin/zxporter-netmon-linux"
echo ""

INSTANCE_NAME=${1:-default}
echo "Running in Lima instance: $INSTANCE_NAME"

# Copy binary to VM
# Limactl copy syntax: limactl cp <source> <instance>:<target>
limactl cp bin/zxporter-netmon-linux $INSTANCE_NAME:/tmp/zxporter-netmon-linux

# Execute in VM
# Using sudo to ensure permissions for conntrack/ebpf
echo "Executing zxporter-netmon in VM..."
limactl shell $INSTANCE_NAME sudo chmod +x /tmp/zxporter-netmon-linux
limactl shell $INSTANCE_NAME sudo /tmp/zxporter-netmon-linux --standalone
