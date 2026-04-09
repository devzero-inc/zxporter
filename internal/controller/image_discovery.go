/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	// podListPageSize is the number of pods fetched per paginated LIST call.
	podListPageSize = 500
)

// NodeImageMap maps nodeName → *NodeBatch.
type NodeImageMap = map[string]*NodeBatch

// WorkloadRefMap maps image digest → list of workload references discovered from pods.
type WorkloadRefMap = map[string][]DiscoveredWorkloadRef

// NodeBatch groups all unique images assigned to a single node for analysis.
type NodeBatch struct {
	NodeName string
	Images   []ImageInfo
}

// ImageInfo represents a unique container image discovered in the cluster.
type ImageInfo struct {
	// Digest is the image ID from container status (e.g. "docker.io/library/nginx@sha256:abc123...")
	Digest string

	// Ref is the full image reference with tag (e.g. "docker.io/library/nginx:1.25.3")
	Ref string

	// ContainerCount tracks how many running containers use this image.
	ContainerCount int
}

// DiscoveredWorkloadRef is the workload reference gathered during discovery.
// It maps to the CRD's WorkloadReference type but is a discovery-time struct
// so we can accumulate container names before writing to the CRD.
type DiscoveredWorkloadRef struct {
	Namespace      string
	WorkloadType   string
	WorkloadName   string
	WorkloadUID    string
	ContainerNames map[string]struct{} // set of container names (deduplicated)
}

// DiscoveryResult holds the complete output of the image discovery phase.
type DiscoveryResult struct {
	// NodeImages maps nodeName → batch of unique images to analyze on that node.
	NodeImages NodeImageMap

	// WorkloadRefs maps image digest → workload references from all pods (global).
	WorkloadRefs WorkloadRefMap

	// TotalPodsScanned is the number of running pods processed.
	TotalPodsScanned int

	// TotalImagesFound is the number of unique images (after dedup).
	TotalImagesFound int

	// TotalContainersSeen is the count of container statuses processed.
	TotalContainersSeen int
}

// ImageDiscoverer lists running pods and builds a node-grouped, deduplicated image map.
type ImageDiscoverer struct {
	client kubernetes.Interface
	config ImageAnalysisConfig
	log    logr.Logger
}

// NewImageDiscoverer creates a new ImageDiscoverer.
func NewImageDiscoverer(client kubernetes.Interface, config ImageAnalysisConfig, log logr.Logger) *ImageDiscoverer {
	return &ImageDiscoverer{
		client: client,
		config: config,
		log:    log.WithName("image-discovery"),
	}
}

// Discover lists all running pods (paginated), extracts container images,
// deduplicates globally by digest, groups by node, and collects workload references.
func (d *ImageDiscoverer) Discover(ctx context.Context) (*DiscoveryResult, error) {
	result := &DiscoveryResult{
		NodeImages:   make(NodeImageMap),
		WorkloadRefs: make(WorkloadRefMap),
	}

	// globalDedup tracks which digests have already been assigned to a node.
	globalDedup := make(map[string]bool)

	// containerCountByDigest tracks total container count per digest.
	containerCountByDigest := make(map[string]int)

	namespaceFilter := d.buildNamespaceFilter()

	continueToken := ""
	for {
		podList, err := d.client.CoreV1().Pods("").List(ctx, metav1.ListOptions{
			Limit:         podListPageSize,
			Continue:      continueToken,
			FieldSelector: "status.phase=Running",
		})
		if err != nil {
			return nil, fmt.Errorf("listing pods: %w", err)
		}

		for i := range podList.Items {
			pod := &podList.Items[i]

			// Skip unscheduled pods.
			if pod.Spec.NodeName == "" {
				continue
			}

			// Apply namespace filter.
			if !namespaceFilter(pod.Namespace) {
				continue
			}

			result.TotalPodsScanned++

			// Process both regular and init container statuses.
			d.processContainerStatuses(pod, pod.Status.ContainerStatuses, false,
				result, globalDedup, containerCountByDigest)
			d.processContainerStatuses(pod, pod.Status.InitContainerStatuses, true,
				result, globalDedup, containerCountByDigest)
		}

		if podList.Continue == "" {
			break
		}
		continueToken = podList.Continue
	}

	// Update container counts on ImageInfo (they were accumulated in containerCountByDigest).
	for _, batch := range result.NodeImages {
		for i := range batch.Images {
			batch.Images[i].ContainerCount = containerCountByDigest[batch.Images[i].Digest]
		}
	}

	d.log.Info("discovery complete",
		"totalPodsScanned", result.TotalPodsScanned,
		"totalContainersSeen", result.TotalContainersSeen,
		"uniqueImages", result.TotalImagesFound,
		"nodes", len(result.NodeImages),
	)

	return result, nil
}

// processContainerStatuses handles a slice of container statuses from a pod.
func (d *ImageDiscoverer) processContainerStatuses(
	pod *corev1.Pod,
	statuses []corev1.ContainerStatus,
	_ bool,
	result *DiscoveryResult,
	globalDedup map[string]bool,
	containerCountByDigest map[string]int,
) {
	nodeName := pod.Spec.NodeName

	for _, cs := range statuses {
		digest := normalizeDigest(cs.ImageID)
		if digest == "" {
			continue
		}

		imageRef := cs.Image
		if imageRef == "" {
			continue
		}

		// Apply image pattern filter.
		if !d.matchesImageFilter(imageRef) {
			continue
		}

		result.TotalContainersSeen++
		containerCountByDigest[digest]++

		// Always collect workload refs globally, even if this image is already deduped.
		d.addWorkloadRef(result.WorkloadRefs, digest, pod, cs.Name)

		// Assign image to first node discovered (global dedup).
		if globalDedup[digest] {
			continue
		}
		globalDedup[digest] = true
		result.TotalImagesFound++

		batch, ok := result.NodeImages[nodeName]
		if !ok {
			batch = &NodeBatch{NodeName: nodeName}
			result.NodeImages[nodeName] = batch
		}
		batch.Images = append(batch.Images, ImageInfo{
			Digest: digest,
			Ref:    imageRef,
		})
	}
}

// addWorkloadRef adds or updates a workload reference for the given image digest.
func (d *ImageDiscoverer) addWorkloadRef(refs WorkloadRefMap, digest string, pod *corev1.Pod, containerName string) {
	workloadName, workloadType, workloadUID := resolveWorkloadOwner(pod)

	// Dedup by namespace/type/name — if same workload already tracked, just add the container name.
	existing := refs[digest]
	for i := range existing {
		if existing[i].Namespace == pod.Namespace &&
			existing[i].WorkloadType == workloadType &&
			existing[i].WorkloadName == workloadName {
			existing[i].ContainerNames[containerName] = struct{}{}
			return
		}
	}

	refs[digest] = append(refs[digest], DiscoveredWorkloadRef{
		Namespace:      pod.Namespace,
		WorkloadType:   workloadType,
		WorkloadName:   workloadName,
		WorkloadUID:    workloadUID,
		ContainerNames: map[string]struct{}{containerName: {}},
	})
}

// resolveWorkloadOwner walks the owner reference chain to find the top-level workload.
// Pod → ReplicaSet → Deployment, Pod → Job → CronJob, etc.
func resolveWorkloadOwner(pod *corev1.Pod) (name, kind, uid string) {
	if len(pod.OwnerReferences) == 0 {
		return pod.Name, "Pod", string(pod.UID)
	}

	owner := pod.OwnerReferences[0]
	switch owner.Kind {
	case "ReplicaSet":
		// ReplicaSets created by Deployments have names like "<deployment>-<hash>"
		// The hash is a 10-char pod-template-hash. Strip it to get the Deployment name.
		rsName := owner.Name
		if idx := strings.LastIndex(rsName, "-"); idx > 0 {
			// This is a Deployment-owned ReplicaSet.
			return rsName[:idx], "Deployment", string(owner.UID)
		}
		// Standalone ReplicaSet (no Deployment owner).
		return rsName, "ReplicaSet", string(owner.UID)

	case "Job":
		// Jobs created by CronJobs have names like "<cronjob>-<unix-timestamp>"
		// The timestamp suffix is 10 digits. If the name has a numeric suffix, assume CronJob.
		jobName := owner.Name
		if idx := strings.LastIndex(jobName, "-"); idx > 0 {
			suffix := jobName[idx+1:]
			if isNumeric(suffix) && len(suffix) >= 8 {
				return jobName[:idx], "CronJob", string(owner.UID)
			}
		}
		return jobName, "Job", string(owner.UID)

	case "StatefulSet":
		return owner.Name, "StatefulSet", string(owner.UID)

	case "DaemonSet":
		return owner.Name, "DaemonSet", string(owner.UID)

	default:
		// Custom CRDs or other controllers.
		return owner.Name, owner.Kind, string(owner.UID)
	}
}

// isNumeric returns true if s consists entirely of digits.
func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// normalizeDigest extracts a consistent digest string from a container status ImageID.
// ImageID can be in formats:
//   - "docker.io/library/nginx@sha256:abc123..."
//   - "sha256:abc123..."
//   - "docker-pullable://docker.io/library/nginx@sha256:abc123..."
//
// We normalize to the full "repo@sha256:..." form when possible, or just the "sha256:..." part.
func normalizeDigest(imageID string) string {
	if imageID == "" {
		return ""
	}

	// Strip "docker-pullable://" prefix if present (older Docker).
	imageID = strings.TrimPrefix(imageID, "docker-pullable://")

	// If it contains "@sha256:", it's already in repo@digest form — use as-is.
	if strings.Contains(imageID, "@sha256:") {
		return imageID
	}

	// If it starts with "sha256:", it's just the digest — use as-is.
	if strings.HasPrefix(imageID, "sha256:") {
		return imageID
	}

	// Unexpected format — return as-is but log.
	return imageID
}

// buildNamespaceFilter returns a function that checks if a namespace should be included.
func (d *ImageDiscoverer) buildNamespaceFilter() func(string) bool {
	targetSet := make(map[string]bool, len(d.config.TargetNamespaces))
	for _, ns := range d.config.TargetNamespaces {
		targetSet[ns] = true
	}

	excludeSet := make(map[string]bool, len(d.config.ExcludedNamespaces))
	for _, ns := range d.config.ExcludedNamespaces {
		excludeSet[ns] = true
	}

	return func(namespace string) bool {
		// If target namespaces are specified, only include those.
		if len(targetSet) > 0 {
			return targetSet[namespace]
		}
		// Otherwise, exclude the excluded namespaces.
		return !excludeSet[namespace]
	}
}

// matchesImageFilter checks if an image reference passes the include/exclude glob filters.
func (d *ImageDiscoverer) matchesImageFilter(imageRef string) bool {
	// If included images are specified, the image must match at least one.
	if len(d.config.IncludedImages) > 0 {
		for _, pattern := range d.config.IncludedImages {
			if matched, _ := path.Match(pattern, imageRef); matched {
				return true
			}
			// Also try matching just the image:tag part (without registry prefix).
			if matched, _ := path.Match(pattern, shortImageRef(imageRef)); matched {
				return true
			}
		}
		return false
	}

	// Check excluded images.
	for _, pattern := range d.config.ExcludedImages {
		if matched, _ := path.Match(pattern, imageRef); matched {
			return false
		}
		if matched, _ := path.Match(pattern, shortImageRef(imageRef)); matched {
			return false
		}
	}

	return true
}

// shortImageRef strips the registry prefix from an image reference.
// "docker.io/library/nginx:1.25.3" → "nginx:1.25.3"
// "registry.k8s.io/pause:3.9" → "pause:3.9"
func shortImageRef(ref string) string {
	parts := strings.Split(ref, "/")
	return parts[len(parts)-1]
}
