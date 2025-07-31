package logger

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/protobuf/types/known/timestamppb"

	gen "github.com/devzero-inc/zxporter/gen/api/v1"
)

// MetricsCollectorClient defines the necessary gRPC method for sending logs.
// Forcefully had to do this as, was not fully convinced to put this in transport layer
// and cant use the DackerClient/DirectSender interface here for circular dependency
type TelemetryLogSender interface {
	SendTelemetryLogs(ctx context.Context, in *gen.SendTelemetryLogsRequest) (*gen.SendTelemetryLogsResponse, error)
}

// Logger is the interface that collectors will use to report critical errors.
// It decouples the collectors from the concrete implementation.
type Logger interface {
	Report(level gen.LogLevel, source string, msg string, err error, fields map[string]string)
	Stop()
}

// Config holds the configuration for the telemetry logger.
type Config struct {
	BatchSize     int           // Max number of logs to buffer before sending.
	FlushInterval time.Duration // Max time to wait before sending a batch.
	SendTimeout   time.Duration // timeout for the gRPC send operation, we need to think about this little more.
	QueueSize     int           // The size of the internal log entry queue.
}

// logger is the concrete implementation of the Logger interface.
type logger struct {
	client   TelemetryLogSender
	config   Config
	logQueue chan *gen.LogEntry
	// clusterID string
	// teamID    string

	stopOnce sync.Once
	stopCh   chan struct{}
	wg       sync.WaitGroup

	// For simple rate limiting to avoid log storms
	rateLimiter     map[uint64]time.Time
	rateLimitMutex  sync.Mutex
	rateLimitWindow time.Duration

	zapLogger *zap.Logger
}

// NewLogger creates and starts a new telemetry logger.
func NewLogger(ctx context.Context, client TelemetryLogSender, config Config, zapLogger *zap.Logger) Logger {
	l := &logger{
		client:   client,
		config:   config,
		logQueue: make(chan *gen.LogEntry, config.QueueSize),
		// clusterID:       clusterID,
		// teamID:          teamID,
		stopCh:          make(chan struct{}),
		rateLimiter:     make(map[uint64]time.Time),
		rateLimitWindow: 1 * time.Minute, // Deduplicate identical errors for 1 minute
		zapLogger:       zapLogger.Named("telemetry_logger"),
	}

	l.wg.Add(1)
	go l.run(ctx)
	return l
}

// Report sends a log entry to the processing queue. It is non-blocking.
func (l *logger) Report(level gen.LogLevel, source string, msg string, err error, fields map[string]string) {
	errorString := ""
	if err != nil {
		errorString = err.Error()
	}
	logHash := hash(source, msg, errorString)

	l.rateLimitMutex.Lock()
	if lastSent, found := l.rateLimiter[logHash]; found && time.Since(lastSent) < l.rateLimitWindow {
		l.rateLimitMutex.Unlock()
		return // Drop the log as it was sent recently.
	}
	l.rateLimiter[logHash] = time.Now()
	l.rateLimitMutex.Unlock()

	entry := &gen.LogEntry{
		Timestamp: timestamppb.Now(),
		Level:     level,
		Source:    source,
		Message:   msg,
		Error:     errorString,
		Fields:    fields,
	}

	select {
	case l.logQueue <- entry:
		// Log successfully queued
	default:
		// The queue is full, indicating the backend is slow or down.
		l.zapLogger.Warn("Telemetry log queue is full. Dropping log.", zap.String("source", source))
	}
}

// Stop shuts down the logger, flushing remained buffered logs.
func (l *logger) Stop() {
	l.stopOnce.Do(func() {
		l.zapLogger.Info("Stopping telemetry logger...")
		close(l.stopCh)
		l.wg.Wait()
		l.zapLogger.Info("Telemetry logger stopped.")
	})
}

// run is the background worker that batches logs and sends them to control plane
func (l *logger) run(ctx context.Context) {
	defer l.wg.Done()
	batch := make([]*gen.LogEntry, 0, l.config.BatchSize)
	ticker := time.NewTicker(l.config.FlushInterval)
	defer ticker.Stop()

	for {
		select {
		case logEntry, ok := <-l.logQueue:
			if !ok {
				// Channel closed, flush remaining batch and exit
				if len(batch) > 0 {
					l.flush(ctx, batch)
				}
				return
			}
			batch = append(batch, logEntry)
			if len(batch) >= l.config.BatchSize {
				l.flush(ctx, batch)
				batch = make([]*gen.LogEntry, 0, l.config.BatchSize)
			}
		case <-ticker.C:
			if len(batch) > 0 {
				l.flush(ctx, batch)
				batch = make([]*gen.LogEntry, 0, l.config.BatchSize)
			}
		case <-l.stopCh:
			// flush the queue one last time before exiting
			close(l.logQueue)
		}
	}
}

// flush sends a batch of logs to the gRPC endpoint.
func (l *logger) flush(ctx context.Context, batch []*gen.LogEntry) {
	if len(batch) == 0 {
		return
	}
	l.zapLogger.Debug("Flushing telemetry log batch", zap.Int("batch_size", len(batch)))
	req := &gen.SendTelemetryLogsRequest{
		// ClusterId: l.clusterID, // no good way to get cluster id and tema id here.. (maybe we can get this things in server side more easily)
		// TeamId:    l.teamID,
		Logs: batch,
	}
	// ctx, cancel := context.WithTimeout(ctx, l.config.SendTimeout)
	// defer cancel() // not sure if we need this
	if _, err := l.client.SendTelemetryLogs(ctx, req); err != nil {
		l.zapLogger.Error("Failed to send telemetry logs", zap.Error(err))
	}
}

// A stupid hashing function for rate limiting. (need to work on this part)
func hash(source, msg, errorString string) uint64 {
	h := uint64(14695981039346656037)
	h = (h ^ uint64(len(source))) * 1099511628211
	for _, c := range source {
		h = (h ^ uint64(c)) * 1099511628211
	}
	h = (h ^ uint64(len(msg))) * 1099511628211
	for _, c := range msg {
		h = (h ^ uint64(c)) * 1099511628211
	}
	h = (h ^ uint64(len(errorString))) * 1099511628211
	for _, c := range errorString {
		h = (h ^ uint64(c)) * 1099511628211
	}
	return h
}
