//go:build linux

package networkmonitor

import (
	"github.com/go-logr/logr"
)

func NewCiliumClient(log logr.Logger, clockSource ClockSource) (Client, error) {
	maps := initMaps()
	// We can't really verify maps here easily without more helper functions from egressd,
	// but the linux implementation will try to use them.
	return &ciliumClient{
		log:         log,
		maps:        maps,
		clockSource: clockSource,
	}, nil
}

func CiliumAvailable(mode string) bool {
	if mode == "cilium" {
		return true
	}
	return bpfMapsExist()
}

type ciliumClient struct {
	log         logrusLogger // wrapping logr to match potential interface or just using logr
	maps        []interface{}
	clockSource ClockSource
}

// We need a helper to adapt logr to what we need or just use logr directly.
// The code in linux implementation uses `c.log`
type logrusLogger = logr.Logger

func (c *ciliumClient) Close() error {
	return nil
}

func (c *ciliumClient) ListEntries(filter EntriesFilter) ([]*Entry, error) {
	// Re-using the timeDiff calculation
	// Note: getKernelHZ and others are in cilium_kernel_hz.go (which should be linux-only or guarded)
	// Actually I put them in cilium_kernel_hz.go without build tag, but it uses /proc which is linux.
	// I should add build tag to cilium_kernel_hz.go
	return listRecords(c.maps, c.clockSource, filter)
}
