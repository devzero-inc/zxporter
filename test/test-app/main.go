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
	data []byte
	mu   sync.Mutex
}

func (mc *MemoryConsumer) adjustMemory(targetMB int) {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	targetBytes := targetMB * 1024 * 1024

	if len(mc.data) < targetBytes {
		mc.data = append(mc.data, make([]byte, targetBytes-len(mc.data))...)
	} else if len(mc.data) > targetBytes {
		mc.data = mc.data[:targetBytes]
	}
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

		// Get current target
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

	// Clamp between 0.02 and 0.03 (2-3%) per core
	if target < 0.02 {
		target = 0.02
	} else if target > 0.03 {
		target = 0.03
	}
	cc.targetUsage = target
}

func main() {
	memConsumer := &MemoryConsumer{}
	cpuConsumer := &CPUConsumer{
		targetUsage: 0.025, // Start at 2.5% CPU per core
	}

	// Limit to single core to better control CPU usage
	runtime.GOMAXPROCS(1)

	// Start CPU consumption
	go cpuConsumer.consumeCPU()

	// Control loop
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		// Memory adjustment
		currentMemory := memConsumer.getCurrentMemoryMB()
		if currentMemory < 45 {
			memConsumer.adjustMemory(50)
		} else if currentMemory > 55 {
			memConsumer.adjustMemory(50)
		}

		// Get current CPU target
		cpuConsumer.mu.Lock()
		cpuTarget := cpuConsumer.targetUsage
		cpuConsumer.mu.Unlock()

		fmt.Printf("Memory Usage: %.2f MB, CPU Target: %.3f%%\n",
			currentMemory, cpuTarget*100)
	}
}
