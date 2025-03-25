// internal/util/logger.go
package util

import (
	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"
)

// NewLogger creates a new logger with the given name
func NewLogger(name string) logr.Logger {
	return ctrl.Log.WithName(name)
}
