// internal/transport/interface.go
package transport

import (
	"context"

	"github.com/devzero-inc/zxporter/internal/collector"
)

// DakrClient defines methods for sending data to Dakr
type DakrClient interface {
	// SendResource sends any resource data to Dakr
	SendResource(ctx context.Context, resource collector.CollectedResource) (string, error)
}

// Sender defines methods for sending data to external systems
type Sender interface {
	// Send transmits a resource to the target system
	Send(ctx context.Context, resource collector.CollectedResource) (string, error)

	// Start initializes the sender (establishing connections, etc.)
	Start(ctx context.Context) error

	// Stop cleans up resources
	Stop() error
}

// // BufferedSender adds buffering capabilities to handle connection issues
// type BufferedSender interface {
// 	Sender

// 	// GetBufferSize returns the current number of items in the buffer
// 	GetBufferSize() int

// 	// Flush attempts to send all buffered items
// 	Flush(ctx context.Context) error
// }
