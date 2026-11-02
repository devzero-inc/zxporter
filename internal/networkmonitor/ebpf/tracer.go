package ebpf

import "github.com/go-logr/logr"

// dnsBatchSize is how many events accumulate before the reader hands them over.
// It lives here rather than beside the reader because NewTracer sizes the channel
// from it, and NewTracer is built on every platform.
const dnsBatchSize = 64

type Config struct {
	// QueueSize is how many DNS events may be buffered on the way to the consumer.
	QueueSize         int
	CustomBTFFilePath string
}

func NewTracer(log logr.Logger, cfg Config) *Tracer {
	if cfg.QueueSize == 0 {
		cfg.QueueSize = 1000
	}
	// The channel carries batches, so its depth is in batches; QueueSize stays
	// denominated in events so that the bound on how much is buffered -- which is what
	// a caller setting it is choosing -- does not silently grow by a factor of
	// dnsBatchSize when the batching changes.
	depth := cfg.QueueSize / dnsBatchSize
	if depth < 1 {
		depth = 1
	}
	return &Tracer{
		log:    log.WithName("ebpf_tracer"),
		cfg:    cfg,
		events: make(chan []DNSEvent, depth),
	}
}

type Tracer struct {
	log    logr.Logger
	cfg    Config
	events chan []DNSEvent
	objs   *bpfObjects
}
