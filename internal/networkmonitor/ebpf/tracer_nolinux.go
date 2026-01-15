//go:build !linux

package ebpf

import (
	"context"

	"github.com/go-logr/logr"
)

func (t *Tracer) Run(ctx context.Context) error {
	// Don't panic, just return error to be safe
	t.log.Info("eBPF tracer not implemented on non-linux systems")
	<-ctx.Done()
	return nil
}

func (t *Tracer) Events() <-chan DNSEvent {
	return t.events
}

func IsKernelBTFAvailable() bool {
	return false
}

func InitCgroupv2(log logr.Logger) error {
	return nil
}

func (t *Tracer) CollectNetworkSummary() (map[NetworkTrafficKey]TrafficSummary, error) {
	return nil, nil
}
