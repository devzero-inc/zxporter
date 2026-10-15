//go:build !linux

package socketprobe

import (
	"context"

	"github.com/go-logr/logr"

	"github.com/devzero-inc/zxporter/internal/networkmonitor/redmetrics"
)

// Probe is the non-Linux stand-in. It exists so the monitor compiles on a
// developer machine; every operation reports the platform as unsupported.
type Probe struct {
	log logr.Logger
}

// New builds an unattached probe.
func New(log logr.Logger) *Probe {
	return &Probe{log: log.WithName("service_request_probe")}
}

// Attach always fails off Linux, which is how the caller learns to deregister the
// health component rather than retry.
func (p *Probe) Attach() error { return ErrUnsupported }

// Run never starts off Linux.
func (p *Probe) Run(_ context.Context, _ func(redmetrics.SocketEvent)) error {
	return ErrUnsupported
}

// Close is a no-op off Linux.
func (p *Probe) Close() error { return nil }
