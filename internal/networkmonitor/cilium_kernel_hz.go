//go:build linux

package networkmonitor

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"time"
)

// Available CONFIG_HZ values, sorted from highest to lowest.
var hzValues = []uint16{1000, 300, 250, 100}

// getKernelHZ attempts to estimate the kernel's CONFIG_HZ compile-time value
func getKernelHZ() (uint16, error) {
	f, err := os.Open("/proc/schedstat")
	if err != nil {
		return 0, err
	}
	defer f.Close()

	j1, err := readSchedstat(f)
	if err != nil {
		return 0, err
	}

	time.Sleep(time.Millisecond * 100)

	j2, err := readSchedstat(f)
	if err != nil {
		return 0, err
	}

	hz, err := j1.interpolate(j2)
	if err != nil {
		return 0, fmt.Errorf("interpolating hz value: %w", err)
	}

	return nearest(hz, hzValues)
}

func readSchedstat(f io.ReadSeeker) (ktime, error) {
	defer func() { _, _ = f.Seek(0, 0) }()

	var j uint64
	var t = time.Now()

	s := bufio.NewScanner(f)
	for s.Scan() {
		if _, err := fmt.Sscanf(s.Text(), "timestamp %d", &j); err == nil {
			return ktime{j, t}, nil
		}
	}

	return ktime{}, errors.New("no kernel timestamp found")
}

type ktime struct {
	k uint64
	t time.Time
}

func (old ktime) interpolate(new ktime) (uint16, error) {
	if old.t.After(new.t) {
		return 0, fmt.Errorf("old wall time %v is more recent than %v", old.t, new.t)
	}
	if old.k > new.k {
		return 0, fmt.Errorf("old kernel timer %d is higher than %d", old.k, new.k)
	}

	kd := new.k - old.k
	td := new.t.Sub(old.t)

	hz := float64(kd) / td.Seconds()
	hz = math.Round(hz)
	if hz > math.MaxUint16 {
		return 0, fmt.Errorf("interpolated hz value would overflow uint16: %f", hz)
	}

	return uint16(hz), nil
}

func nearest(in uint16, values []uint16) (uint16, error) {
	if len(values) == 0 {
		return 0, errors.New("values cannot be empty")
	}

	var out uint16
	min := ^uint16(0)
	for _, v := range values {
		d := uint16(in - v)
		if in < v {
			d = v - in
		}

		if d < min {
			min = d
			out = v
		}
	}

	return out, nil
}
