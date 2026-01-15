package ebpf

import "github.com/go-logr/logr"

type Config struct {
	QueueSize         int
	CustomBTFFilePath string
}

func NewTracer(log logr.Logger, cfg Config) *Tracer {
	if cfg.QueueSize == 0 {
		cfg.QueueSize = 1000
	}
	return &Tracer{
		log:    log.WithName("ebpf_tracer"),
		cfg:    cfg,
		events: make(chan DNSEvent, cfg.QueueSize),
	}
}

type Tracer struct {
	log    logr.Logger
	cfg    Config
	events chan DNSEvent
	objs   *bpfObjects
}
