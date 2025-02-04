package main

import (
	"fmt"
	"math"
	"runtime"
	"sync"
	"time"
)

type MemoryConsumer struct {
	data []byte
}

type CPUConsumer struct {
	stopChan chan struct{}
	wg       sync.WaitGroup
}

func (mc *MemoryConsumer) consumeMemory(megabytes int) {
	mc.data = nil
	runtime.GC()

	mc.data = make([]byte, megabytes*1024*1024)
	for i := range mc.data {
		mc.data[i] = byte(i % 256)
	}
}

func (mc *MemoryConsumer) clear() {
	mc.data = nil
	runtime.GC()
}

func (c *CPUConsumer) consumeCPUMillicores(millicores int) {
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		cpuRatio := float64(millicores) / 1000.0

		const windowSize = 100 * time.Millisecond
		computeDuration := time.Duration(float64(windowSize) * cpuRatio)
		sleepDuration := windowSize - computeDuration

		for {
			select {
			case <-c.stopChan:
				return
			default:
				start := time.Now()
				for time.Since(start) < computeDuration {
					math.Sin(math.Pi / 2)
				}
				time.Sleep(sleepDuration)
			}
		}
	}()
}

func (c *CPUConsumer) stop() {
	close(c.stopChan)
	c.wg.Wait()
	c.stopChan = make(chan struct{})
}

func main() {
	mc := &MemoryConsumer{}
	cpuConsumer := &CPUConsumer{
		stopChan: make(chan struct{}),
	}

	fmt.Println("Phase 1: Consuming 80Mi memory and 80m CPU...")
	mc.consumeMemory(80)
	cpuConsumer.consumeCPUMillicores(80)

	time.Sleep(12 * time.Minute)

	fmt.Println("Stopping Phase 1 and clearing resources...")
	cpuConsumer.stop()
	mc.clear()

	time.Sleep(5 * time.Second)

	fmt.Println("Phase 2: Increasing to 95Mi memory and 95m CPU...")
	mc.consumeMemory(95)
	cpuConsumer.consumeCPUMillicores(95)

	select {}
}
