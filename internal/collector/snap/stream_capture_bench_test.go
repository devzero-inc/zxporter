package snap

import (
	"context"
	"fmt"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	gen "github.com/devzero-inc/zxporter/gen/api/v1"
)

// peakHeap runs fn while sampling HeapAlloc and returns the peak growth over
// the pre-run baseline, in bytes.
func peakHeap(fn func()) uint64 {
	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)

	var peak atomic.Uint64
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(500 * time.Microsecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				var m runtime.MemStats
				runtime.ReadMemStats(&m)
				if m.HeapAlloc > peak.Load() {
					peak.Store(m.HeapAlloc)
				}
			}
		}
	}()

	fn()

	// One final sample to catch the end state.
	var end runtime.MemStats
	runtime.ReadMemStats(&end)
	if end.HeapAlloc > peak.Load() {
		peak.Store(end.HeapAlloc)
	}
	close(stop)
	<-done

	if peak.Load() < base.HeapAlloc {
		return 0
	}
	return peak.Load() - base.HeapAlloc
}

// buildLegacySnapshot materializes a full ClusterSnapshot object graph the
// way the legacy capture does: nodes with per-node pod maps plus namespaces
// with deployment/service maps.
func buildLegacySnapshot(namespaces, deploymentsPerNS, podsPerNS int) *gen.ClusterSnapshot {
	snapshot := &gen.ClusterSnapshot{
		ClusterInfo:   &gen.ClusterInfo{},
		Nodes:         map[string]*gen.NodeData{},
		Namespaces:    map[string]*gen.Namespace{},
		ClusterScoped: &gen.ClusterScopedSnapshot{},
		Timestamp:     timestamppb.Now(),
		SnapshotId:    "bench-snapshot",
	}

	const nodes = 50
	for n := 0; n < nodes; n++ {
		snapshot.Nodes[fmt.Sprintf("node-uid-%d", n)] = &gen.NodeData{
			Node: &gen.ResourceIdentifier{Name: fmt.Sprintf("node-%d", n)},
			Pods: map[string]*gen.ResourceIdentifier{},
		}
	}

	for ns := 0; ns < namespaces; ns++ {
		nsData := &gen.Namespace{
			Namespace:   &gen.ResourceIdentifier{Name: fmt.Sprintf("namespace-%d", ns)},
			Deployments: map[string]*gen.ResourceIdentifier{},
			Services:    map[string]*gen.ResourceIdentifier{},
		}
		for d := 0; d < deploymentsPerNS; d++ {
			uid := fmt.Sprintf("deploy-uid-%d-%d", ns, d)
			nsData.Deployments[uid] = &gen.ResourceIdentifier{Name: fmt.Sprintf("deployment-%d-%d", ns, d)}
		}
		snapshot.Namespaces[fmt.Sprintf("ns-uid-%d", ns)] = nsData

		for p := 0; p < podsPerNS; p++ {
			node := snapshot.Nodes[fmt.Sprintf("node-uid-%d", p%nodes)]
			uid := fmt.Sprintf("pod-uid-%d-%d", ns, p)
			node.Pods[uid] = &gen.ResourceIdentifier{Name: fmt.Sprintf("pod-%d-%d", ns, p)}
		}
	}
	return snapshot
}

// streamingBenchSources emits the same logical entry counts as
// buildLegacySnapshot, in listing-sized pages, without materializing them.
func streamingBenchSources(namespaces, deploymentsPerNS, podsPerNS int) []snapshotSource {
	pagedWalk := func(kind string, perNS, pageSize int) func(ctx context.Context, emit func([]*gen.SnapshotEntry) error) error {
		return func(ctx context.Context, emit func([]*gen.SnapshotEntry) error) error {
			for ns := 0; ns < namespaces; ns++ {
				for start := 0; start < perNS; start += pageSize {
					end := start + pageSize
					if end > perNS {
						end = perNS
					}
					page := make([]*gen.SnapshotEntry, 0, end-start)
					for i := start; i < end; i++ {
						page = append(page, &gen.SnapshotEntry{
							Uid:       fmt.Sprintf("%s-uid-%d-%d", kind, ns, i),
							Name:      fmt.Sprintf("%s-%d-%d", kind, ns, i),
							Namespace: fmt.Sprintf("namespace-%d", ns),
						})
					}
					if err := emit(page); err != nil {
						return err
					}
				}
			}
			return nil
		}
	}

	return []snapshotSource{
		{rt: gen.ResourceType_RESOURCE_TYPE_DEPLOYMENT, name: "deployments", walk: pagedWalk("deploy", deploymentsPerNS, metadataListPageSize)},
		{rt: gen.ResourceType_RESOURCE_TYPE_POD, name: "pods", walk: pagedWalk("pod", podsPerNS, podListPageSize)},
	}
}

// discardBatchStream consumes batches without retaining them, standing in for
// the network in sender-side benchmarks.
type discardBatchStream struct {
	batches int
}

func (d *discardBatchStream) SendBatch(rt gen.ResourceType, entries []*gen.SnapshotEntry, typeComplete bool) error {
	d.batches++
	return nil
}

func (d *discardBatchStream) Finish() (*gen.SendClusterSnapshotBatchedResponse, error) {
	return &gen.SendClusterSnapshotBatchedResponse{Status: "processed"}, nil
}

func (d *discardBatchStream) Abort() {}

// benchScales: namespaces × (deployments, pods) per namespace. Totals:
// small=11k, medium=55k, large=220k entries.
var benchScales = []struct {
	name             string
	namespaces       int
	deploymentsPerNS int
	podsPerNS        int
}{
	{"small-11k", 100, 10, 100},
	{"medium-55k", 500, 10, 100},
	{"large-220k", 2000, 10, 100},
}

// BenchmarkSnapshotPeakMemory_LegacyBuildAndMarshal measures the legacy
// sender path: materialize the full snapshot object graph, marshal it into
// one buffer, slice into 8MB chunks.
func BenchmarkSnapshotPeakMemory_LegacyBuildAndMarshal(b *testing.B) {
	const maxChunkSize = 8 * 1024 * 1024
	for _, scale := range benchScales {
		b.Run(scale.name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				peak := peakHeap(func() {
					snapshot := buildLegacySnapshot(scale.namespaces, scale.deploymentsPerNS, scale.podsPerNS)
					data, err := proto.Marshal(snapshot)
					if err != nil {
						b.Fatal(err)
					}
					chunks := 0
					for start := 0; start < len(data); start += maxChunkSize {
						chunks++
					}
					runtime.KeepAlive(chunks)
					runtime.KeepAlive(snapshot)
				})
				b.ReportMetric(float64(peak), "peak-bytes")
			}
		})
	}
}

// BenchmarkSnapshotPeakMemory_Streaming measures the streaming sender path:
// the same logical entries paged and batched into a discarding stream.
func BenchmarkSnapshotPeakMemory_Streaming(b *testing.B) {
	for _, scale := range benchScales {
		b.Run(scale.name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				peak := peakHeap(func() {
					stream := &discardBatchStream{}
					sources := streamingBenchSources(scale.namespaces, scale.deploymentsPerNS, scale.podsPerNS)
					if _, err := streamSnapshotSources(context.Background(), logr.Discard(), stream, sources, snapshotBatchSize); err != nil {
						b.Fatal(err)
					}
				})
				b.ReportMetric(float64(peak), "peak-bytes")
			}
		})
	}
}
