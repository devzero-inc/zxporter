package provider

import (
	"context"

	"github.com/go-logr/logr"
	"k8s.io/client-go/kubernetes"
)

// detectorOptions holds the optional parameters for the Detector.
type detectorOptions struct {
	KubeContextName string
}

// DetectorOpt is a function that configures a Detector.
type DetectorOpt func(*detectorOptions)

// WithKubeContextName sets the KubeContextName for the Detector.
func WithKubeContextName(name string) DetectorOpt {
	return func(opts *detectorOptions) {
		opts.KubeContextName = name
	}
}

// Detector discovers and instantiates the appropriate cloud provider
type Detector struct {
	logger          logr.Logger
	k8sClient       kubernetes.Interface
	kubeContextName string
}

// NewDetector creates a new provider detector
func NewDetector(logger logr.Logger, k8sClient kubernetes.Interface, opts ...DetectorOpt) *Detector {
	detectorOptions := detectorOptions{}
	for _, opt := range opts {
		opt(&detectorOptions)
	}

	return &Detector{
		logger:          logger,
		k8sClient:       k8sClient,
		kubeContextName: detectorOptions.KubeContextName,
	}
}

// DetectProvider attempts to identify and return the appropriate provider
func (d *Detector) DetectProvider(ctx context.Context) (Provider, error) {
	// Fall back to generic provider
	d.logger.Info("No specific cloud provider detected, using generic provider")
	return NewGenericProvider(d.logger, d.k8sClient, d.kubeContextName), nil
}
