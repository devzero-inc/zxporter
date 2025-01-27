#!/bin/bash

# Function to download kernel if not already present
download_kernel() {
    if [ ! -d "linux-6.1.1" ]; then
        echo "Downloading kernel source..."
        wget https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-6.1.1.tar.xz
        tar xf linux-6.1.1.tar.xz
        cd linux-6.1.1
        make defconfig
        cd ..
    fi
}

# Function to build kernel with specified cores
build_kernel() {
    local cores=$1
    echo "Starting build with $cores cores..."
    cd linux-6.1.1
    make clean
    time make -j$cores 2>&1 | tee "/build/build_log_${cores}_cores.txt"
    cd ..
}

echo "Preparing build environment..."
download_kernel

while true; do
    if [ -f "/build/config/start.txt" ]; then
        CONTENT=$(cat /build/config/start.txt)
        if [ "$CONTENT" != "WAIT" ] && [[ "$CONTENT" =~ ^[0-9,]+$ ]]; then
            echo "Received core configuration: $CONTENT"
            IFS=',' read -ra CORE_ARRAY <<< "$CONTENT"
            
            # Build for each core count
            for cores in "${CORE_ARRAY[@]}"; do
                echo "$(date): Starting build with $cores cores"
                build_kernel $cores
                echo "$(date): Completed build with $cores cores"
            done
            break
        fi
    fi
    echo "Waiting for build configuration... (checking every 5 seconds)"
    sleep 5
done