#!/bin/bash

# CORES=(2 4 8 16)
CORES=(2 4)

echo "Cleaning build directory..."
make clean

for cores in "${CORES[@]}"; do
    echo "Building with $cores cores..."
    start_time=$(date +%s)
    
    make -j$cores
    
    end_time=$(date +%s)
    build_time=$((end_time - start_time))
    
    echo "Build with $cores cores completed in $build_time seconds."
    echo "----------------------------------------"
done
