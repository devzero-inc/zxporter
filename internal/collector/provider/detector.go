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
	d.logger.Info("Detecting cloud provider")

	// Try AWS EKS
	d.logger.Info("Attempting to detect AWS EKS provider")
	awsProvider, err := NewAWSProvider(d.logger, d.k8sClient)
	if err == nil {
		// Try to get metadata to confirm this is AWS
		_, err = awsProvider.GetClusterMetadata(ctx)
		if err == nil {
			d.logger.Info("Detected AWS EKS provider")
			return awsProvider, nil
		}
		d.logger.Info("AWS detection failed", "error", err)
	}

	// Try GCP GKE
	d.logger.Info("Attempting to detect GCP GKE provider")
	gcpProvider, err := NewGCPProvider(d.logger, d.k8sClient)
	if err == nil {
		// Try to get metadata to confirm this is GCP
		_, err = gcpProvider.GetClusterMetadata(ctx)
		if err == nil {
			d.logger.Info("Detected GCP GKE provider")
			return gcpProvider, nil
		}
		d.logger.Info("GCP detection failed", "error", err)
	}

	// Try Azure AKS
	d.logger.Info("Attempting to detect Azure AKS provider")
	azureProvider, err := NewAzureProvider(d.logger, d.k8sClient)
	if err == nil {
		// Try to get metadata to confirm this is Azure
		_, err = azureProvider.GetClusterMetadata(ctx)
		if err == nil {
			d.logger.Info("Detected Azure AKS provider")
			return azureProvider, nil
		}
		d.logger.Info("Azure detection failed", "error", err)
	}

	// Fall back to generic provider
	d.logger.Info("No specific cloud provider detected, using generic provider")
	return NewGenericProvider(d.logger, d.k8sClient), nil
}
