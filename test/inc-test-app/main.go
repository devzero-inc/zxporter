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
	data          []byte
	mu            sync.Mutex
	currentTarget int // current target in MB
}

func (mc *MemoryConsumer) adjustMemory(targetMB int) {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	targetBytes := targetMB * 1024 * 1024
	mc.currentTarget = targetMB

	// Clear old slice to help garbage collection
	mc.data = nil
	runtime.GC() // Force garbage collection

	// Allocate new slice
	mc.data = make([]byte, targetBytes)
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

	// Cap at 400m (0.4 cores)
	if target > 0.4 {
		target = 0.4
	}
	cc.targetUsage = target
}

func main() {
	memConsumer := &MemoryConsumer{
		currentTarget: 40, // Start with 40MB
	}
	cpuConsumer := &CPUConsumer{
		targetUsage: 0.045, // Start at 4.5% CPU
	}

	// Limit to single core to better control CPU usage
	runtime.GOMAXPROCS(1)

	// Initial memory allocation
	memConsumer.adjustMemory(memConsumer.currentTarget)

	// Start CPU consumption
	go cpuConsumer.consumeCPU()

	// Control loop for resource adjustment
	minuteTicker := time.NewTicker(1 * time.Minute)
	statsTicker := time.NewTicker(1 * time.Second)
	defer minuteTicker.Stop()
	defer statsTicker.Stop()

	for {
		select {
		case <-minuteTicker.C:
			// Increase memory target by 1.15x
			newMemoryTarget := int(float64(memConsumer.currentTarget) * 1.15)
			if newMemoryTarget > 400 {
				newMemoryTarget = 400
			}
			memConsumer.adjustMemory(newMemoryTarget)

			// Increase CPU target by 1.15x
			cpuConsumer.mu.Lock()
			newCPUTarget := cpuConsumer.targetUsage * 1.15
			cpuConsumer.mu.Unlock()
			cpuConsumer.adjustUsage(newCPUTarget)

		case <-statsTicker.C:
			// Print current stats
			currentMemory := memConsumer.getCurrentMemoryMB()
			cpuConsumer.mu.Lock()
			cpuTarget := cpuConsumer.targetUsage
			cpuConsumer.mu.Unlock()

			fmt.Printf("Memory Usage: %.2f MB (Target: %d MB), CPU Target: %.1f%%\n",
				currentMemory,
				memConsumer.currentTarget,
				cpuTarget*100)
		}
	}
}
