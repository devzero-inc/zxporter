```go
// Package recommender provides interfaces for resource recommendation in Kubernetes
package recommender

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// ResourceType represents different types of resources that can be recommended
type ResourceType string

const (
	ResourceCPU    ResourceType = "cpu"
	ResourceMemory ResourceType = "memory"
	ResourceGPU    ResourceType = "gpu"
)

// MetricsProvider interface for fetching resource metrics
type MetricsProvider interface {
	GetMetricsRange(ctx context.Context, query string, start, end time.Time, step time.Duration) ([]float64, error)
	QueryAtTime(query string, t time.Time) (float64, error)
}

// ResourceMetrics holds the metrics data for a specific resource
type ResourceMetrics struct {
	ResourceType ResourceType
	Current     float64
	Historical  []float64
	Timestamp   time.Time
}

// RecommendationContext holds all the data needed for making recommendations
type RecommendationContext struct {
	Pod            *corev1.Pod
	Container      *corev1.Container
	ResourceType   ResourceType
	MetricsData    map[ResourceType]ResourceMetrics
	Config         ResourceConfig
	MetricsProvider MetricsProvider
}

// ResourceConfig holds configuration for resource recommendations
type ResourceConfig struct {
	RequestPercentile  float64
	MarginFraction    float64
	TargetUtilization float64
	MinRecommendation resource.Quantity
	MaxRecommendation resource.Quantity
	BufferSize        resource.Quantity
}

// Recommendation represents a resource recommendation
type Recommendation struct {
	ResourceType          ResourceType
	CurrentUsage         float64
	BaseRecommendation   resource.Quantity
	FinalRecommendation  resource.Quantity
	Confidence          float64
	RecommendationReason string
}

// ResourceRecommender is the main interface for making recommendations
type ResourceRecommender interface {
	// Core functionality
	Name() string
	Validate(ctx context.Context, rc *RecommendationContext) error
	
	// Pipeline phases
	PreProcess(ctx context.Context, rc *RecommendationContext) error
	CollectMetrics(ctx context.Context, rc *RecommendationContext) error
	AnalyzeMetrics(ctx context.Context, rc *RecommendationContext) error
	GenerateRecommendation(ctx context.Context, rc *RecommendationContext) (*Recommendation, error)
	PostProcess(ctx context.Context, rc *RecommendationContext, rec *Recommendation) error
	
	// Monitoring and validation
	ValidateRecommendation(ctx context.Context, rec *Recommendation) error
	ObserveRecommendation(ctx context.Context, rec *Recommendation) error
}

// BaseRecommender provides default implementations for ResourceRecommender
type BaseRecommender struct {
	resourceType ResourceType
	config      ResourceConfig
}

// ResourceSpecificRecommender interface for resource-specific logic
type ResourceSpecificRecommender interface {
	ResourceRecommender
	GetResourceSpecificMetrics(ctx context.Context, rc *RecommendationContext) error
	ApplyResourceConstraints(ctx context.Context, rec *Recommendation) error
}

// CPURecommender implements resource-specific logic for CPU
type CPURecommender struct {
	BaseRecommender
}

// MemoryRecommender implements resource-specific logic for Memory
type MemoryRecommender struct {
	BaseRecommender
	OOMProtection bool
	OOMBumpRatio  float64
}

// GPURecommender implements resource-specific logic for GPU
type GPURecommender struct {
	BaseRecommender
	GPUUtilizationThreshold float64
	GPUMemoryThreshold     float64
}

// RecommenderFactory creates resource-specific recommenders
type RecommenderFactory interface {
	CreateRecommender(resourceType ResourceType, config ResourceConfig) (ResourceRecommender, error)
}

// RecommendationEngine orchestrates the recommendation process
type RecommendationEngine struct {
	recommenders map[ResourceType]ResourceRecommender
	factory     RecommenderFactory
}

// RecommendationPolicy defines how recommendations should be applied
type RecommendationPolicy struct {
	AutoApply           bool
	GracePeriod         time.Duration
	MaxChangePercent    float64
	MinChangeThreshold  float64
	CooldownPeriod     time.Duration
}

// RecommendationResult contains the final recommendation results
type RecommendationResult struct {
	Recommendations []Recommendation
	Policy         RecommendationPolicy
	Timestamp      time.Time
	Status         string
	Message        string
}
```