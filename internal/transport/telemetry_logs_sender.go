package transport

import (
	"context"
	"fmt"
	"time"

	gen "github.com/devzero-inc/zxporter/gen/api/v1"
)

type telemetryLogsSender struct {
	dakrClient DakrClient

	logChan chan *gen.LogEntry

	ctx    context.Context
	cancel context.CancelFunc
}

func NewTelemetryLogsSender(
	dakrClient DakrClient,
	logChan chan *gen.LogEntry,
	parentCtx context.Context,
) *telemetryLogsSender {
	ctx, cancel := context.WithCancel(parentCtx)
	s := &telemetryLogsSender{
		dakrClient: dakrClient,
		logChan:    logChan,
		ctx:        ctx,
		cancel:     cancel,
	}

	go s.run()

	return s
}

func (s *telemetryLogsSender) run() {
	buffer := make([]*gen.LogEntry, 0, 20)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case log := <-s.logChan:
			buffer = append(buffer, log)
			if len(buffer) >= 20 {
				s.flush(buffer)
				buffer = buffer[:0]
			}
		case <-ticker.C:
			if len(buffer) > 0 {
				s.flush(buffer)
				buffer = buffer[:0]
			}
		}
	}
}

func (s *telemetryLogsSender) flush(logs []*gen.LogEntry) {
	err := s.dakrClient.SendTelemetryLogs(s.ctx, logs...)
	if err != nil {
		fmt.Printf("SendTelemetryLogs failed: %v\n", err)
	}
}
