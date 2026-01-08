//go:build !linux

package networkmonitor

import (
	"errors"

	"github.com/go-logr/logr"
)

type ClockSource string

const (
	ClockSourceKtime   ClockSource = "ktime"
	ClockSourceJiffies ClockSource = "jiffies"
)

func NewCiliumClient(log logr.Logger, clockSource ClockSource) (Client, error) {
	return nil, errors.New("cilium client not supported on non-linux")
}

func CiliumAvailable(mode string) bool {
	return false
}
