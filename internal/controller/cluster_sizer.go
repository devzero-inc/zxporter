package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/util/retry"
	resourcehelper "k8s.io/component-helpers/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/devzero-inc/zxporter/internal/health"
)

const (
	// ClusterSizerDeploymentName is the Deployment the sizer resizes: the
	// controller's own. The RBAC Role grants get/patch on this name only.
	ClusterSizerDeploymentName = "devzero-zxporter-controller-manager"
	// ClusterSizerAnnotation holds the sizing ladder as JSON. Its absence
	// disables the sizer, so Helm opts in simply by rendering it.
	ClusterSizerAnnotation = "devzero.io/cluster-sizer"

	defaultClusterSizerPollInterval   = 5 * time.Minute
	defaultClusterSizerScaleDownDelay = time.Hour
	minClusterSizerPollInterval       = 30 * time.Second
	clusterSizerNodePageSize          = 500

	kindRequests                      = "requests"
	kindLimits                        = "limits"
	defaultClusterSizerMaxNodePercent = 50
)

// ClusterSizerConfig is the annotation payload: a ladder per container and
// resource, plus the shared timing and node-fit settings.
type ClusterSizerConfig struct {
	PollInterval   string `json:"pollInterval,omitempty"`
	ScaleDownDelay string `json:"scaleDownDelay,omitempty"`
	// MaxNodePercent caps every sized request at this share of the largest
	// allocatable among nodes the pod can schedule on, so a resize never asks
	// for more than any node can give and stalls the rollout. Default 50.
	MaxNodePercent int                             `json:"maxNodePercent,omitempty"`
	Containers     map[string]ContainerSizerConfig `json:"containers"`
}

// ContainerSizerConfig maps resource names (memory, cpu) to their ladder, for
// requests and limits independently. A resource that is not listed is never
// touched.
type ContainerSizerConfig struct {
	Requests map[corev1.ResourceName]ResourceLadder `json:"requests,omitempty"`
	Limits   map[corev1.ResourceName]ResourceLadder `json:"limits,omitempty"`
}

// ResourceLadder is one scaling function:
//
//	want = min(max, base + step * max(ceil(nodes/nodesPerStep), ceil(cores/coresPerStep)))
//
// A zero NodesPerStep or CoresPerStep disables that term, and a nil Max means
// no cap.
type ResourceLadder struct {
	Base         *resource.Quantity `json:"base,omitempty"`
	Step         *resource.Quantity `json:"step,omitempty"`
	Max          *resource.Quantity `json:"max,omitempty"`
	NodesPerStep int                `json:"nodesPerStep,omitempty"`
	CoresPerStep int                `json:"coresPerStep,omitempty"`
}

// ClusterSize is what the ladder is evaluated against: every node, schedulable
// or not, and the sum of their CPU capacity in whole cores.
// Fit is separate: the largest allocatable per resource among the nodes the
// sized pod could actually land on, which bounds what a request may ask for.
type ClusterSize struct {
	Nodes int
	Cores int
	Fit   corev1.ResourceList
	// Eligible holds the allocatable of every node the pod could land on, so
	// the resized pod can be checked against one real node rather than
	// against Fit, whose per-resource maxima may come from different nodes.
	Eligible []corev1.ResourceList
}

type parsedSizerConfig struct {
	pollInterval   time.Duration
	scaleDownDelay time.Duration
	maxNodePercent int
	containers     map[string]ContainerSizerConfig
}

// ClusterSizer keeps the controller's own resources proportional to cluster
// size. It scales up as soon as the ladder asks for more, and scales down only
// after the lower value has held for the configured delay, so a cluster that
// bounces across a step boundary does not restart zxporter on every tick.
type ClusterSizer struct {
	client        client.Client
	apiReader     client.Reader
	namespace     string
	healthManager *health.HealthManager
	now           func() time.Time

	mu sync.Mutex
	// downSince records when each target first asked for less than it has,
	// keyed by sizerKey. Cleared as soon as the target stops asking for less.
	// It is in-memory on purpose: a restart or leader change can only delay a
	// scale-down, never cause one early.
	downSince map[string]time.Time
}

// NewClusterSizer builds the sizer. apiReader must be uncached
// (mgr.GetAPIReader()): nodes are listed directly each tick so the sizer adds
// no node informer to the very process whose memory it is sizing.
func NewClusterSizer(
	c client.Client,
	apiReader client.Reader,
	namespace string,
	healthManager *health.HealthManager,
) *ClusterSizer {
	return &ClusterSizer{
		client:        c,
		apiReader:     apiReader,
		namespace:     namespace,
		healthManager: healthManager,
		now:           time.Now,
		downSince:     map[string]time.Time{},
	}
}

// Start implements the controller-manager Runnable interface. The interval is
// re-read from the annotation after every pass, so a changed pollInterval
// takes effect without a restart.
func (s *ClusterSizer) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("cluster-sizer")
	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-timer.C:
			next, err := s.ReconcileOnce(ctx)
			if err != nil {
				logger.Error(err, "failed to size controller to cluster")
			}
			timer.Reset(next)
		case <-ctx.Done():
			return nil
		}
	}
}

// NeedLeaderElection makes the sizer run only on the elected manager.
func (s *ClusterSizer) NeedLeaderElection() bool { return true }

// ReconcileOnce sizes the Deployment once and returns how long to wait before
// the next pass.
func (s *ClusterSizer) ReconcileOnce(ctx context.Context) (time.Duration, error) {
	next := defaultClusterSizerPollInterval
	var size ClusterSize
	var result string
	var applied map[string]string

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		applied = nil
		var deploy appsv1.Deployment
		// Uncached like the node list: a cached Get would start a
		// cluster-wide Deployment informer even with sizing disabled, and a
		// stale read would turn every conflict retry into another conflict.
		if err := s.apiReader.Get(ctx, client.ObjectKey{Name: ClusterSizerDeploymentName, Namespace: s.namespace}, &deploy); err != nil {
			if apierrors.IsNotFound(err) {
				result = "deployment_missing"
				return nil
			}
			return err
		}

		raw, ok := deploy.Annotations[ClusterSizerAnnotation]
		if !ok || raw == "" {
			s.resetPending()
			result = "disabled"
			return nil
		}
		cfg, err := parseClusterSizerConfig(raw)
		if err != nil {
			return err
		}
		next = cfg.pollInterval

		size, err = s.clusterSize(ctx, &deploy.Spec.Template.Spec)
		if err != nil {
			return err
		}
		if size.Nodes == 0 {
			// An empty node list is a transient API answer, not a cluster
			// that shrank to nothing; sizing against it would scale down.
			result = "no_nodes"
			return nil
		}

		base := deploy.DeepCopy()
		applied, result, err = s.plan(&deploy, cfg, size)
		if err != nil || len(applied) == 0 {
			return err
		}
		if err := s.client.Patch(ctx, &deploy, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		s.updateHealth(health.HealthStatusDegraded, size, "error", nil, err)
		return next, err
	}
	if len(applied) > 0 {
		log.FromContext(ctx).WithName("cluster-sizer").Info("resized controller to cluster size",
			"nodes", size.Nodes, "cores", size.Cores, "resources", applied)
	}
	s.updateHealth(health.HealthStatusHealthy, size, result, applied, nil)
	return next, nil
}

// plan mutates deploy in place with every change that is due now and returns
// those changes (as "container/requests/memory" -> "768Mi") with the result
// label. Nothing in deploy changes when the returned map is empty.
func (s *ClusterSizer) plan(deploy *appsv1.Deployment, cfg parsedSizerConfig, size ClusterSize) (map[string]string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	applied := map[string]string{}
	pending := false
	seen := map[string]bool{}

	containers := deploy.Spec.Template.Spec.Containers
	base := deploy.Spec.Template.Spec.DeepCopy().Containers
	for name, ctrCfg := range cfg.containers {
		idx := -1
		for i := range containers {
			if containers[i].Name == name {
				idx = i
				break
			}
		}
		if idx < 0 {
			return nil, "", fmt.Errorf("container %q in %s is not in deployment %s", name, ClusterSizerAnnotation, ClusterSizerDeploymentName)
		}
		ctr := &containers[idx]

		// Limits first, so a request planned below sees the limit it will
		// be patched alongside.
		for _, kind := range []string{kindLimits, kindRequests} {
			ladders := ctrCfg.Requests
			if kind == kindLimits {
				ladders = ctrCfg.Limits
			}
			for res, ladder := range ladders {
				want := ladder.Calculate(size, res)
				// Only requests decide scheduling, so a limit above node
				// size is harmless, unless the container has no request for
				// it: the API server then defaults the request to the limit.
				_, hasRequest := ctr.Resources.Requests[res]
				_, sizesRequest := ctrCfg.Requests[res]
				if kind == kindRequests || (!hasRequest && !sizesRequest) {
					fit := size.Fit[res]
					if fit.Sign() <= 0 {
						// No node the pod can land on reports this
						// resource: there is nothing to size against, and
						// treating it as unlimited could strand the rollout.
						return nil, "", fmt.Errorf("no schedulable node reports allocatable %s", res)
					}
					capped := quantityFor(res, milli(&fit)*int64(cfg.maxNodePercent)/100)
					if want.Cmp(capped) > 0 {
						want = capped
					}
				}
				// Checked after the cap, which can round a small
				// allocatable down to zero.
				if want.IsZero() {
					continue
				}
				key := name + "/" + kind + "/" + string(res)
				seen[key] = true

				list := resourceList(ctr, kind)
				current, has := (*list)[res]
				switch cmp := want.Cmp(current); {
				case !has || cmp > 0:
					delete(s.downSince, key)
				case cmp == 0:
					delete(s.downSince, key)
					continue
				default:
					since, ok := s.downSince[key]
					if !ok {
						s.downSince[key] = now
						pending = true
						continue
					}
					if now.Sub(since) < cfg.scaleDownDelay {
						pending = true
						continue
					}
					// Due. The timer stays until a later pass sees the
					// patched value in place (cmp == 0), so a conflict
					// retry of this pass does not restart the delay.
				}
				if *list == nil {
					*list = corev1.ResourceList{}
				}
				(*list)[res] = want
				applied[key] = want.String()
			}
		}
	}
	keepRequestsWithinLimits(containers, base, cfg, applied)
	if len(applied) > 0 {
		if req := podRequests(&deploy.Spec.Template.Spec); !fitsOneNode(req, size.Eligible) {
			return nil, "", fmt.Errorf("resized pod requests %v fit no schedulable node", req)
		}
	}

	// A ladder removed from the config must not leave a stale timer behind
	// that would fire the moment it is added back.
	for key := range s.downSince {
		if !seen[key] {
			delete(s.downSince, key)
		}
	}

	switch {
	case len(applied) > 0:
		return applied, "patched", nil
	case pending:
		return nil, "scale_down_pending", nil
	default:
		return nil, "unchanged", nil
	}
}

// keepRequestsWithinLimits repairs any request the plan left above its
// container's limit, which the API server would reject along with the whole
// patch. A sized request is lowered to the limit; otherwise the limit is the
// sized side and is raised to the request, so a resource the config does not
// list is never changed. applied is updated to match.
func keepRequestsWithinLimits(containers, base []corev1.Container, cfg parsedSizerConfig, applied map[string]string) {
	for i := range containers {
		ctr := &containers[i]
		ctrCfg, ok := cfg.containers[ctr.Name]
		if !ok {
			continue
		}
		for res, req := range ctr.Resources.Requests {
			lim, ok := ctr.Resources.Limits[res]
			if !ok || req.Cmp(lim) <= 0 {
				continue
			}
			if _, sized := ctrCfg.Requests[res]; !sized {
				setPlanned(ctr, &base[i], kindLimits, res, req, applied)
				continue
			}
			// Lower the request to the limit, but never below what it
			// holds now unless its own scale-down is due: a limit whose
			// delay expired must not carry the request past its delay.
			// The limit is held up at the retained request instead.
			value := lim
			orig, hadRequest := base[i].Resources.Requests[res]
			_, dueNow := applied[ctr.Name+"/"+kindRequests+"/"+string(res)]
			if hadRequest && !(dueNow && req.Cmp(orig) < 0) && value.Cmp(orig) < 0 {
				value = orig
			}
			setPlanned(ctr, &base[i], kindRequests, res, value, applied)
			if value.Cmp(lim) > 0 {
				setPlanned(ctr, &base[i], kindLimits, res, value, applied)
			}
		}
	}
}

// setPlanned writes value into the container and keeps applied in step with
// what now differs from the deployment as it was read.
func setPlanned(ctr, base *corev1.Container, kind string, res corev1.ResourceName, value resource.Quantity, applied map[string]string) {
	(*resourceList(ctr, kind))[res] = value
	key := ctr.Name + "/" + kind + "/" + string(res)
	if orig, ok := (*resourceList(base, kind))[res]; ok && orig.Cmp(value) == 0 {
		delete(applied, key)
	} else {
		applied[key] = value.String()
	}
}

// podRequests is what the scheduler reserves for the pod, computed by the
// scheduler's own helper (native sidecars, init containers, overhead). A
// container with a limit but no request for a resource gets the request the
// API server would default it to.
func podRequests(spec *corev1.PodSpec) corev1.ResourceList {
	pod := &corev1.Pod{Spec: *spec.DeepCopy()}
	for _, ctrs := range [][]corev1.Container{pod.Spec.InitContainers, pod.Spec.Containers} {
		for i := range ctrs {
			r := &ctrs[i].Resources
			for res, lim := range r.Limits {
				if _, ok := r.Requests[res]; !ok {
					if r.Requests == nil {
						r.Requests = corev1.ResourceList{}
					}
					r.Requests[res] = lim.DeepCopy()
				}
			}
		}
	}
	return resourcehelper.PodRequests(pod, resourcehelper.PodResourcesOptions{})
}

// fitsOneNode reports whether a single node's allocatable covers every
// requested resource. Free capacity is not considered; maxNodePercent is the
// headroom for what already runs there.
func fitsOneNode(req corev1.ResourceList, nodes []corev1.ResourceList) bool {
	for _, alloc := range nodes {
		fits := true
		for res, q := range req {
			if q.Sign() <= 0 {
				continue
			}
			if a, ok := alloc[res]; !ok || q.Cmp(a) > 0 {
				fits = false
				break
			}
		}
		if fits {
			return true
		}
	}
	return false
}

func resourceList(ctr *corev1.Container, kind string) *corev1.ResourceList {
	if kind == kindLimits {
		return &ctr.Resources.Limits
	}
	return &ctr.Resources.Requests
}

func (s *ClusterSizer) resetPending() {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.downSince)
}

// clusterSize lists every node page by page through the uncached reader.
// Affinity is not evaluated for Fit; nodeSelector, taints and cordons are.
func (s *ClusterSizer) clusterSize(ctx context.Context, pod *corev1.PodSpec) (ClusterSize, error) {
	size := ClusterSize{Fit: corev1.ResourceList{}}
	var cores resource.Quantity
	continueToken := ""
	for {
		var nodes corev1.NodeList
		opts := []client.ListOption{client.Limit(clusterSizerNodePageSize)}
		if continueToken != "" {
			opts = append(opts, client.Continue(continueToken))
		}
		if err := s.apiReader.List(ctx, &nodes, opts...); err != nil {
			return ClusterSize{}, fmt.Errorf("list nodes: %w", err)
		}
		size.Nodes += len(nodes.Items)
		for i := range nodes.Items {
			node := &nodes.Items[i]
			cores.Add(node.Status.Capacity[corev1.ResourceCPU])
			if !podFitsNode(pod, node) {
				continue
			}
			size.Eligible = append(size.Eligible, node.Status.Allocatable)
			for res, q := range node.Status.Allocatable {
				if cur, ok := size.Fit[res]; !ok || q.Cmp(cur) > 0 {
					size.Fit[res] = q.DeepCopy()
				}
			}
		}
		continueToken = nodes.Continue
		if continueToken == "" {
			break
		}
	}
	// Round fractional capacity up, as a 15.5-core cluster has more than 15.
	size.Cores = int((cores.MilliValue() + 999) / 1000)
	return size, nil
}

// Calculate evaluates the ladder for one resource. Arithmetic is in
// milli-units so CPU steps below one core work; the result is formatted the
// way that resource is conventionally written (768Mi, 250m).
func (l ResourceLadder) Calculate(size ClusterSize, res corev1.ResourceName) resource.Quantity {
	base := milli(l.Base)
	step := milli(l.Step)
	steps := max(increments(size.Nodes, l.NodesPerStep), increments(size.Cores, l.CoresPerStep))
	want := base + step*int64(steps)
	if l.Max != nil && want > milli(l.Max) {
		want = milli(l.Max)
	}
	return quantityFor(res, want)
}

// quantityFor formats a milli-unit amount the way the resource is
// conventionally written: CPU in millicores, byte-like resources in whole
// units with binary suffixes.
func quantityFor(res corev1.ResourceName, m int64) resource.Quantity {
	if res == corev1.ResourceCPU {
		return *resource.NewMilliQuantity(m, resource.DecimalSI)
	}
	return *resource.NewQuantity((m+999)/1000, resource.BinarySI)
}

// podFitsNode reports whether the pod could be scheduled on the node at all:
// not cordoned, nodeSelector matched, and every scheduling taint tolerated.
func podFitsNode(pod *corev1.PodSpec, node *corev1.Node) bool {
	if node.Spec.Unschedulable {
		return false
	}
	for k, v := range pod.NodeSelector {
		// An empty value still requires the label to exist.
		if got, ok := node.Labels[k]; !ok || got != v {
			return false
		}
	}
	for i := range node.Spec.Taints {
		taint := &node.Spec.Taints[i]
		if taint.Effect == corev1.TaintEffectPreferNoSchedule {
			continue
		}
		tolerated := false
		for j := range pod.Tolerations {
			if pod.Tolerations[j].ToleratesTaint(logr.Discard(), taint, false) {
				tolerated = true
				break
			}
		}
		if !tolerated {
			return false
		}
	}
	return true
}

func milli(q *resource.Quantity) int64 {
	if q == nil {
		return 0
	}
	return q.MilliValue()
}

// increments is ceil(count/per), and 0 when per is 0 (term disabled).
func increments(count, per int) int {
	if per <= 0 {
		return 0
	}
	return (count + per - 1) / per
}

func parseClusterSizerConfig(raw string) (parsedSizerConfig, error) {
	var cfg ClusterSizerConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return parsedSizerConfig{}, fmt.Errorf("parse %s: %w", ClusterSizerAnnotation, err)
	}
	out := parsedSizerConfig{
		pollInterval:   defaultClusterSizerPollInterval,
		scaleDownDelay: defaultClusterSizerScaleDownDelay,
		maxNodePercent: defaultClusterSizerMaxNodePercent,
		containers:     cfg.Containers,
	}
	switch {
	case cfg.MaxNodePercent < 0 || cfg.MaxNodePercent > 100:
		return parsedSizerConfig{}, fmt.Errorf("%s maxNodePercent must be between 1 and 100", ClusterSizerAnnotation)
	case cfg.MaxNodePercent > 0:
		out.maxNodePercent = cfg.MaxNodePercent
	}
	var err error
	if cfg.PollInterval != "" {
		if out.pollInterval, err = time.ParseDuration(cfg.PollInterval); err != nil {
			return parsedSizerConfig{}, fmt.Errorf("parse %s pollInterval: %w", ClusterSizerAnnotation, err)
		}
		out.pollInterval = max(out.pollInterval, minClusterSizerPollInterval)
	}
	if cfg.ScaleDownDelay != "" {
		if out.scaleDownDelay, err = time.ParseDuration(cfg.ScaleDownDelay); err != nil {
			return parsedSizerConfig{}, fmt.Errorf("parse %s scaleDownDelay: %w", ClusterSizerAnnotation, err)
		}
		if out.scaleDownDelay < 0 {
			return parsedSizerConfig{}, fmt.Errorf("%s scaleDownDelay must not be negative", ClusterSizerAnnotation)
		}
	}
	if len(cfg.Containers) == 0 {
		return parsedSizerConfig{}, fmt.Errorf("%s must configure at least one container", ClusterSizerAnnotation)
	}
	for name, ctr := range cfg.Containers {
		// A null or empty entry (left behind by helm --reuse-values) would
		// otherwise report healthy and size nothing, forever.
		if len(ctr.Requests) == 0 && len(ctr.Limits) == 0 {
			return parsedSizerConfig{}, fmt.Errorf("%s container %q has no requests or limits to size", ClusterSizerAnnotation, name)
		}
		for kind, ladders := range map[string]map[corev1.ResourceName]ResourceLadder{kindRequests: ctr.Requests, kindLimits: ctr.Limits} {
			for res, l := range ladders {
				if err := l.validate(); err != nil {
					return parsedSizerConfig{}, fmt.Errorf("%s %s.%s.%s: %w", ClusterSizerAnnotation, name, kind, res, err)
				}
			}
		}
	}
	return out, nil
}

func (l ResourceLadder) validate() error {
	for label, q := range map[string]*resource.Quantity{"base": l.Base, "step": l.Step, "max": l.Max} {
		if q != nil && q.Sign() < 0 {
			return fmt.Errorf("%s must not be negative", label)
		}
	}
	if l.NodesPerStep < 0 || l.CoresPerStep < 0 {
		return fmt.Errorf("nodesPerStep and coresPerStep must not be negative")
	}
	// A ladder with neither always computes zero and would be skipped
	// silently. `helm upgrade --reuse-values` from a chart without the
	// clusterSizer block produces exactly that when only nodesPerStep is set.
	if milli(l.Base) == 0 && milli(l.Step) == 0 {
		return fmt.Errorf("sets neither base nor step")
	}
	if milli(l.Step) > 0 && l.NodesPerStep == 0 && l.CoresPerStep == 0 {
		return fmt.Errorf("step is set but neither nodesPerStep nor coresPerStep is")
	}
	if l.Max != nil && l.Base != nil && l.Max.Cmp(*l.Base) < 0 {
		return fmt.Errorf("max %s is below base %s", l.Max, l.Base)
	}
	return nil
}

func (s *ClusterSizer) updateHealth(status health.HealthStatus, size ClusterSize, result string, applied map[string]string, err error) {
	if s.healthManager == nil {
		return
	}
	message := result
	errorMessage := ""
	if err != nil {
		errorMessage = err.Error()
		message = errorMessage
	}
	metadata := map[string]string{
		"nodes":  strconv.Itoa(size.Nodes),
		"cores":  strconv.Itoa(size.Cores),
		"result": result,
		"error":  errorMessage,
	}
	keys := make([]string, 0, len(applied))
	for k := range applied {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		metadata["applied:"+k] = applied[k]
	}
	for res, q := range size.Fit {
		if res == corev1.ResourceCPU || res == corev1.ResourceMemory {
			metadata["fit:"+string(res)] = q.String()
		}
	}
	s.healthManager.UpdateStatus(health.ComponentClusterSizer, status, message, metadata)
}
