//go:build linux

package networkmonitor

import (
	"fmt"

	"github.com/cilium/cilium/pkg/datapath/linux/probes"
	"golang.org/x/sys/unix"
)

var hertz uint16

func init() {
	var err error
	hertz, err = getKernelHZ()
	if err != nil {
		hertz = 1
	}
}

type ClockSource string

const (
	ClockSourceKtime   ClockSource = "ktime"
	ClockSourceJiffies ClockSource = "jiffies"
)

// kernelTimeDiffSecondsFunc returns time diff function based on clock source.
func kernelTimeDiffSecondsFunc(clockSource ClockSource) (func(t int64) int64, error) {
	switch clockSource {
	case ClockSourceKtime:
		now, err := getMtime()
		if err != nil {
			return nil, err
		}
		now = now / 1000000000
		return func(t int64) int64 {
			return t - int64(now)
		}, nil
	case ClockSourceJiffies:
		now, err := probes.Jiffies()
		if err != nil {
			return nil, err
		}
		return func(t int64) int64 {
			diff := t - int64(now)
			diff = diff << 8
			diff = diff / int64(hertz)
			return diff
		}, nil
	default:
		return nil, fmt.Errorf("unknown clock source %q", clockSource)
	}
}

func getMtime() (uint64, error) {
	var ts unix.Timespec

	err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts)
	if err != nil {
		return 0, fmt.Errorf("Unable get time: %w", err)
	}

	return uint64(unix.TimespecToNsec(ts)), nil
}
