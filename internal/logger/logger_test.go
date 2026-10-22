package logger

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"go.uber.org/zap"

	gen "github.com/devzero-inc/zxporter/gen/api/v1"
)

// recordingSender records every batch the logger sends.
type recordingSender struct {
	mu   sync.Mutex
	logs []*gen.LogEntry
}

func (s *recordingSender) SendTelemetryLogs(
	_ context.Context,
	in *gen.SendTelemetryLogsRequest,
) (*gen.SendTelemetryLogsResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logs = append(s.logs, in.Logs...)
	return &gen.SendTelemetryLogsResponse{}, nil
}

func (s *recordingSender) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.logs)
}

func newTestLogger(sender TelemetryLogSender) Logger {
	return NewLogger(context.Background(), sender, Config{
		BatchSize:     100,
		FlushInterval: time.Hour, // only Stop flushes
		QueueSize:     100,
	}, zap.NewNop(), logr.Discard())
}

// Stop is what the reconciler calls when cluster registration with dakr
// fails; the collectors keep reporting after it. Neither may panic: a panic
// turns a dakr outage into a crash loop.
func TestReportAfterStopDoesNotPanic(t *testing.T) {
	// Many instances: the stop path used to close the queue on every select
	// that picked the (still closed) stop channel, so a second close panicked
	// only when the select chose it again.
	for i := 0; i < 200; i++ {
		l := newTestLogger(&recordingSender{})
		l.Stop()
		l.Stop()
		l.Report(gen.LogLevel_LOG_LEVEL_ERROR, "test", fmt.Sprintf("after stop %d", i), errors.New("boom"), nil)
	}
}

func TestStopFlushesQueuedLogs(t *testing.T) {
	sender := &recordingSender{}
	l := newTestLogger(sender)
	for i := 0; i < 5; i++ {
		l.Report(gen.LogLevel_LOG_LEVEL_ERROR, "test", fmt.Sprintf("queued %d", i), nil, nil)
	}
	l.Stop()
	if got := sender.count(); got != 5 {
		t.Fatalf("Stop flushed %d of 5 queued logs", got)
	}
}

func TestReportConcurrentWithStop(t *testing.T) {
	for i := 0; i < 50; i++ {
		l := newTestLogger(&recordingSender{})
		var wg sync.WaitGroup
		for g := 0; g < 4; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				for j := 0; j < 50; j++ {
					l.Report(gen.LogLevel_LOG_LEVEL_ERROR, "test", fmt.Sprintf("g%d-%d", g, j), nil, nil)
				}
			}(g)
		}
		l.Stop()
		wg.Wait()
	}
}
