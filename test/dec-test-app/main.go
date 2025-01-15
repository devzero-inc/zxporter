package main

import (
	"fmt"
	"math"
	"runtime"
	"sync"
	"time"
)

// MemoryConsumer holds memory consuming data
type MemoryConsumer struct {
	data          [][]byte // Use array of arrays for better memory control
	mu            sync.Mutex
	currentTarget int // current target in MB
}

func (mc *MemoryConsumer) adjustMemory(targetMB int) {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	// Don't go below 50MB
	if targetMB < 50 {
		targetMB = 50
	}

	// Clear existing data first
	mc.data = nil
	runtime.GC()
	time.Sleep(100 * time.Millisecond) // Give GC time to work

	// Allocate memory in 1MB chunks for better control
	chunkSize := 1 * 1024 * 1024 // 1MB chunks
	numChunks := targetMB

	mc.data = make([][]byte, numChunks)
	for i := 0; i < numChunks; i++ {
		mc.data[i] = make([]byte, chunkSize)
		// Write some data to ensure memory is actually allocated
		for j := 0; j < len(mc.data[i]); j += 4096 {
			mc.data[i][j] = byte(j % 256)
		}
	}

	mc.currentTarget = targetMB
}

func (mc *MemoryConsumer) getCurrentMemoryMB() float64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return float64(m.Alloc) / 1024 / 1024
}

// CPUConsumer performs CPU-intensive calculations
type CPUConsumer struct {
	targetUsage float64 // target CPU usage (0-1)
	mu          sync.Mutex
}

func (cc *CPUConsumer) consumeCPU() {
	for {
		start := time.Now()

		cc.mu.Lock()
		target := cc.targetUsage
		cc.mu.Unlock()

		// Busy wait for targetMs milliseconds
		busyMs := time.Duration(target*100) * time.Millisecond
		for time.Since(start) < busyMs {
			math.Sin(2.0)
		}

		// Sleep for the remainder of 100ms
		elapsed := time.Since(start)
		sleepTime := (100 * time.Millisecond) - elapsed
		if sleepTime > 0 {
			time.Sleep(sleepTime)
		}
	}
}

func (cc *CPUConsumer) adjustUsage(target float64) {
	cc.mu.Lock()
	defer cc.mu.Unlock()

	// Don't go below 50m (0.05 cores)
	if target < 0.05 {
		target = 0.05
	}
	cc.targetUsage = target
}

func main() {
	memConsumer := &MemoryConsumer{
		currentTarget: 400, // Start with 400MB
	}
	cpuConsumer := &CPUConsumer{
		targetUsage: 0.4, // Start at 400m CPU
	}

	// Limit to single core to better control CPU usage
	runtime.GOMAXPROCS(1)

	// Initial memory allocation
	memConsumer.adjustMemory(memConsumer.currentTarget)

	// Start CPU consumption
	go cpuConsumer.consumeCPU()

	// Control loop for resource adjustment
	minuteTicker := time.NewTicker(30 * time.Second) // Adjust every 30 seconds for testing
	statsTicker := time.NewTicker(1 * time.Second)
	defer minuteTicker.Stop()
	defer statsTicker.Stop()

	startTime := time.Now()
	iteration := 0

	for {
		select {
		case <-minuteTicker.C:
			iteration++
			currentMemory := memConsumer.getCurrentMemoryMB()

			// Calculate new targets
			newMemoryTarget := int(float64(memConsumer.currentTarget) / 1.5)
			if newMemoryTarget < 50 {
				newMemoryTarget = 50
			}

			// Adjust CPU
			cpuConsumer.mu.Lock()
			newCPUTarget := cpuConsumer.targetUsage / 1.5
			if newCPUTarget < 0.05 {
				newCPUTarget = 0.05
			}
			cpuConsumer.mu.Unlock()
			cpuConsumer.adjustUsage(newCPUTarget)

			// Force garbage collection before adjusting memory
			runtime.GC()

			// Adjust memory with the new target
			memConsumer.adjustMemory(newMemoryTarget)

			fmt.Printf("\nIteration %d - Adjusting resources:\n", iteration)
			fmt.Printf("Current Memory: %.2f MB, New Target: %d MB\n", currentMemory, newMemoryTarget)
			fmt.Printf("New CPU Target: %.1f%%\n", newCPUTarget*100)

		case <-statsTicker.C:
			currentMemory := memConsumer.getCurrentMemoryMB()
			cpuConsumer.mu.Lock()
			cpuTarget := cpuConsumer.targetUsage
			cpuConsumer.mu.Unlock()

			elapsed := time.Since(startTime)
			fmt.Printf("[%02d:%02d] Memory: %.2f MB (Target: %d MB), CPU: %.1f%%\n",
				int(elapsed.Minutes()),
				int(elapsed.Seconds())%60,
				currentMemory,
				memConsumer.currentTarget,
				cpuTarget*100)
		}
	}
}
