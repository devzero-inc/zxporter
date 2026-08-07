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

package e2e

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	. "github.com/onsi/ginkgo/v2" //nolint:golint,revive
	. "github.com/onsi/gomega"    //nolint:golint,revive

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/devzero-inc/zxporter/internal/nodemon"
)

// gpuMigFixtureNamespace is separate from the "controller" suite's namespace
// and from oom-flow's oomVictimNamespace: this spec doesn't deploy the
// zxporter manager either, it drives nodemon's exporter code directly
// in-process against real DCGM-compatible HTTP endpoints running on the
// cluster.
const gpuMigFixtureNamespace = "gpu-mig-e2e"

// simulatedGpuNodePoolLabel matches run-ai/fake-gpu-operator's
// topology.nodePoolLabelKey default (see .github/actions/gpu-mig-kind/action.yml,
// which installs the chart and labels a worker node with this key so its
// real status-exporter DaemonSet schedules there and serves genuine,
// live-simulated non-MIG DCGM metrics for gpuPlainNodePool).
const simulatedGpuNodePoolLabel = "run.ai/simulated-gpu-node-pool"

const gpuPlainNodePool = "default"

// migFixtureMetrics is real DCGM Prometheus exposition text reproducing the
// shape of a real customer node that surfaced this bug on a p4d.24xlarge
// (8x A100): 3 whole GPUs (2 actively claimed by application workloads, 1
// idle/unrequested) plus 5 MIG-partitioned physical GPUs carrying 12x
// "1g.5gb" + 7x "3g.20gb" instances, matching that node's live NodePool
// status.resources
// (nvidia.com/gpu: 3, nvidia.com/mig-1g.5gb: 12, nvidia.com/mig-3g.20gb: 7)
// exactly — 8 physical GPUs, 22 DCGM rows. All 19 MIG slices are idle:
// the investigation confirmed neither workload on that node requests a
// MIG-typed resource, so they're unclaimed capacity, not active load.
//
// Byte-shaped after run-ai/fake-gpu-operator's own published samples
// (design/samples/<2.9/mig/metrics/*.ini) and its status-exporter's actual
// metric/label set (internal/status-exporter/export/metrics/metrics.go,
// design/MIG Metrics.md) — see gpu_dcgm_exposition_e2e_test.go for the same
// fixture driven through the parser without a cluster, and
// TestGPUExposition_MIG_EightPhysicalGPUsTwentyTwoRows there for the exact expected
// values this spec also asserts.
//
// A live cluster's worth of fidelity for the MIG case specifically comes
// from a static fixture, not a running simulator, because
// run-ai/fake-gpu-operator v0.2.0's status-exporter does not implement MIG
// instance emission at all: its metrics.go GaugeVecs don't declare
// GPU_I_PROFILE/GPU_I_ID as label dimensions, and no file in
// internal/status-exporter references those labels — confirmed by reading
// the shipped source, not assumed from its (aspirational) design doc. The
// non-MIG node in this spec uses the real operator; this fixture stands in
// only for the gap in what it can currently simulate.
//
//nolint:lll // real DCGM exposition text — the label set can't be wrapped without breaking the fixture
const migFixtureMetrics = `# HELP DCGM_FI_DEV_GPU_UTIL GPU utilization (in %).
# TYPE DCGM_FI_DEV_GPU_UTIL gauge
DCGM_FI_DEV_GPU_UTIL{gpu="0",UUID="GPU-a0000000-0000-0000-0000-000000000000",device="nvidia0",modelName="NVIDIA A100-SXM4-40GB",Hostname="gpu-mig-fixture-node",container="model-eval",namespace="batch-ml",pod="model-eval-job-7d9f4c8b6-x2k9p"} 90
DCGM_FI_DEV_GPU_UTIL{gpu="1",UUID="GPU-a0000001-0000-0000-0000-000000000000",device="nvidia1",modelName="NVIDIA A100-SXM4-40GB",Hostname="gpu-mig-fixture-node",container="inference",namespace="serving-ml",pod="inference-svc-6b8f9c7d5-m4n2q"} 15
DCGM_FI_DEV_GPU_UTIL{gpu="2",UUID="GPU-a0000002-0000-0000-0000-000000000000",device="nvidia2",modelName="NVIDIA A100-SXM4-40GB",Hostname="gpu-mig-fixture-node",container="",namespace="",pod=""} 0
# HELP DCGM_FI_DEV_FB_USED Framebuffer memory used (in MiB).
# TYPE DCGM_FI_DEV_FB_USED gauge
DCGM_FI_DEV_FB_USED{gpu="0",UUID="GPU-a0000000-0000-0000-0000-000000000000",device="nvidia0",modelName="NVIDIA A100-SXM4-40GB",Hostname="gpu-mig-fixture-node",container="model-eval",namespace="batch-ml",pod="model-eval-job-7d9f4c8b6-x2k9p"} 39000
DCGM_FI_DEV_FB_USED{gpu="1",UUID="GPU-a0000001-0000-0000-0000-000000000000",device="nvidia1",modelName="NVIDIA A100-SXM4-40GB",Hostname="gpu-mig-fixture-node",container="inference",namespace="serving-ml",pod="inference-svc-6b8f9c7d5-m4n2q"} 36000
DCGM_FI_DEV_FB_USED{gpu="2",UUID="GPU-a0000002-0000-0000-0000-000000000000",device="nvidia2",modelName="NVIDIA A100-SXM4-40GB",Hostname="gpu-mig-fixture-node",container="",namespace="",pod=""} 0
DCGM_FI_DEV_FB_USED{gpu="3",UUID="GPU-b0000000-0000-0000-0000-000000000000",device="nvidia3",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="1g.5gb",GPU_I_ID="8",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 6
DCGM_FI_DEV_FB_USED{gpu="3",UUID="GPU-b0000000-0000-0000-0000-000000000000",device="nvidia3",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="3g.20gb",GPU_I_ID="9",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 18
DCGM_FI_DEV_FB_USED{gpu="3",UUID="GPU-b0000000-0000-0000-0000-000000000000",device="nvidia3",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="3g.20gb",GPU_I_ID="10",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 18
DCGM_FI_DEV_FB_USED{gpu="4",UUID="GPU-b0000001-0000-0000-0000-000000000000",device="nvidia4",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="1g.5gb",GPU_I_ID="11",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 6
DCGM_FI_DEV_FB_USED{gpu="4",UUID="GPU-b0000001-0000-0000-0000-000000000000",device="nvidia4",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="3g.20gb",GPU_I_ID="12",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 18
DCGM_FI_DEV_FB_USED{gpu="4",UUID="GPU-b0000001-0000-0000-0000-000000000000",device="nvidia4",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="3g.20gb",GPU_I_ID="13",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 18
DCGM_FI_DEV_FB_USED{gpu="5",UUID="GPU-b0000002-0000-0000-0000-000000000000",device="nvidia5",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="1g.5gb",GPU_I_ID="14",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 6
DCGM_FI_DEV_FB_USED{gpu="5",UUID="GPU-b0000002-0000-0000-0000-000000000000",device="nvidia5",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="1g.5gb",GPU_I_ID="15",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 6
DCGM_FI_DEV_FB_USED{gpu="5",UUID="GPU-b0000002-0000-0000-0000-000000000000",device="nvidia5",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="1g.5gb",GPU_I_ID="16",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 6
DCGM_FI_DEV_FB_USED{gpu="5",UUID="GPU-b0000002-0000-0000-0000-000000000000",device="nvidia5",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="1g.5gb",GPU_I_ID="17",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 6
DCGM_FI_DEV_FB_USED{gpu="5",UUID="GPU-b0000002-0000-0000-0000-000000000000",device="nvidia5",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="3g.20gb",GPU_I_ID="18",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 18
DCGM_FI_DEV_FB_USED{gpu="6",UUID="GPU-b0000003-0000-0000-0000-000000000000",device="nvidia6",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="1g.5gb",GPU_I_ID="19",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 6
DCGM_FI_DEV_FB_USED{gpu="6",UUID="GPU-b0000003-0000-0000-0000-000000000000",device="nvidia6",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="1g.5gb",GPU_I_ID="20",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 6
DCGM_FI_DEV_FB_USED{gpu="6",UUID="GPU-b0000003-0000-0000-0000-000000000000",device="nvidia6",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="1g.5gb",GPU_I_ID="21",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 6
DCGM_FI_DEV_FB_USED{gpu="6",UUID="GPU-b0000003-0000-0000-0000-000000000000",device="nvidia6",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="3g.20gb",GPU_I_ID="22",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 18
DCGM_FI_DEV_FB_USED{gpu="7",UUID="GPU-b0000004-0000-0000-0000-000000000000",device="nvidia7",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="1g.5gb",GPU_I_ID="23",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 6
DCGM_FI_DEV_FB_USED{gpu="7",UUID="GPU-b0000004-0000-0000-0000-000000000000",device="nvidia7",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="1g.5gb",GPU_I_ID="24",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 6
DCGM_FI_DEV_FB_USED{gpu="7",UUID="GPU-b0000004-0000-0000-0000-000000000000",device="nvidia7",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="1g.5gb",GPU_I_ID="25",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 6
DCGM_FI_DEV_FB_USED{gpu="7",UUID="GPU-b0000004-0000-0000-0000-000000000000",device="nvidia7",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="3g.20gb",GPU_I_ID="26",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 18
# HELP DCGM_FI_DEV_FB_FREE Framebuffer memory free (in MiB).
# TYPE DCGM_FI_DEV_FB_FREE gauge
DCGM_FI_DEV_FB_FREE{gpu="0",UUID="GPU-a0000000-0000-0000-0000-000000000000",device="nvidia0",modelName="NVIDIA A100-SXM4-40GB",Hostname="gpu-mig-fixture-node",container="model-eval",namespace="batch-ml",pod="model-eval-job-7d9f4c8b6-x2k9p"} 1536
DCGM_FI_DEV_FB_FREE{gpu="1",UUID="GPU-a0000001-0000-0000-0000-000000000000",device="nvidia1",modelName="NVIDIA A100-SXM4-40GB",Hostname="gpu-mig-fixture-node",container="inference",namespace="serving-ml",pod="inference-svc-6b8f9c7d5-m4n2q"} 4536
DCGM_FI_DEV_FB_FREE{gpu="2",UUID="GPU-a0000002-0000-0000-0000-000000000000",device="nvidia2",modelName="NVIDIA A100-SXM4-40GB",Hostname="gpu-mig-fixture-node",container="",namespace="",pod=""} 40536
DCGM_FI_DEV_FB_FREE{gpu="3",UUID="GPU-b0000000-0000-0000-0000-000000000000",device="nvidia3",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="1g.5gb",GPU_I_ID="8",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 4857
DCGM_FI_DEV_FB_FREE{gpu="3",UUID="GPU-b0000000-0000-0000-0000-000000000000",device="nvidia3",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="3g.20gb",GPU_I_ID="9",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 20462
DCGM_FI_DEV_FB_FREE{gpu="3",UUID="GPU-b0000000-0000-0000-0000-000000000000",device="nvidia3",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="3g.20gb",GPU_I_ID="10",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 20462
DCGM_FI_DEV_FB_FREE{gpu="4",UUID="GPU-b0000001-0000-0000-0000-000000000000",device="nvidia4",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="1g.5gb",GPU_I_ID="11",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 4857
DCGM_FI_DEV_FB_FREE{gpu="4",UUID="GPU-b0000001-0000-0000-0000-000000000000",device="nvidia4",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="3g.20gb",GPU_I_ID="12",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 20462
DCGM_FI_DEV_FB_FREE{gpu="4",UUID="GPU-b0000001-0000-0000-0000-000000000000",device="nvidia4",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="3g.20gb",GPU_I_ID="13",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 20462
DCGM_FI_DEV_FB_FREE{gpu="5",UUID="GPU-b0000002-0000-0000-0000-000000000000",device="nvidia5",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="1g.5gb",GPU_I_ID="14",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 4857
DCGM_FI_DEV_FB_FREE{gpu="5",UUID="GPU-b0000002-0000-0000-0000-000000000000",device="nvidia5",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="1g.5gb",GPU_I_ID="15",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 4857
DCGM_FI_DEV_FB_FREE{gpu="5",UUID="GPU-b0000002-0000-0000-0000-000000000000",device="nvidia5",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="1g.5gb",GPU_I_ID="16",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 4857
DCGM_FI_DEV_FB_FREE{gpu="5",UUID="GPU-b0000002-0000-0000-0000-000000000000",device="nvidia5",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="1g.5gb",GPU_I_ID="17",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 4857
DCGM_FI_DEV_FB_FREE{gpu="5",UUID="GPU-b0000002-0000-0000-0000-000000000000",device="nvidia5",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="3g.20gb",GPU_I_ID="18",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 20462
DCGM_FI_DEV_FB_FREE{gpu="6",UUID="GPU-b0000003-0000-0000-0000-000000000000",device="nvidia6",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="1g.5gb",GPU_I_ID="19",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 4857
DCGM_FI_DEV_FB_FREE{gpu="6",UUID="GPU-b0000003-0000-0000-0000-000000000000",device="nvidia6",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="1g.5gb",GPU_I_ID="20",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 4857
DCGM_FI_DEV_FB_FREE{gpu="6",UUID="GPU-b0000003-0000-0000-0000-000000000000",device="nvidia6",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="1g.5gb",GPU_I_ID="21",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 4857
DCGM_FI_DEV_FB_FREE{gpu="6",UUID="GPU-b0000003-0000-0000-0000-000000000000",device="nvidia6",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="3g.20gb",GPU_I_ID="22",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 20462
DCGM_FI_DEV_FB_FREE{gpu="7",UUID="GPU-b0000004-0000-0000-0000-000000000000",device="nvidia7",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="1g.5gb",GPU_I_ID="23",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 4857
DCGM_FI_DEV_FB_FREE{gpu="7",UUID="GPU-b0000004-0000-0000-0000-000000000000",device="nvidia7",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="1g.5gb",GPU_I_ID="24",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 4857
DCGM_FI_DEV_FB_FREE{gpu="7",UUID="GPU-b0000004-0000-0000-0000-000000000000",device="nvidia7",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="1g.5gb",GPU_I_ID="25",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 4857
DCGM_FI_DEV_FB_FREE{gpu="7",UUID="GPU-b0000004-0000-0000-0000-000000000000",device="nvidia7",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="3g.20gb",GPU_I_ID="26",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 20462
`

// Labeled "gpu-mig" so CI can select just this spec
// (`--ginkgo.label-filter=gpu-mig`) without the pre-existing "controller"
// Describe block, which needs Prometheus/cert-manager and a full `make
// deploy` this suite doesn't use. See .github/actions/gpu-mig-kind for the
// cluster-level setup (kind, run-ai/fake-gpu-operator) this spec assumes is
// already in place before it runs.
var _ = Describe("GPU node summary (MIG and non-MIG)", Ordered, Label("gpu-mig"), func() {
	var (
		clientset  kubernetes.Interface
		dynClient  dynamic.Interface
		restConfig *rest.Config
		log        logr.Logger
	)

	BeforeAll(func() {
		restConfig = ctrl.GetConfigOrDie()

		var err error
		clientset, err = kubernetes.NewForConfig(restConfig)
		Expect(err).NotTo(HaveOccurred())
		dynClient, err = dynamic.NewForConfig(restConfig)
		Expect(err).NotTo(HaveOccurred())

		zapLog, _ := zap.NewDevelopment()
		log = zapr.NewLogger(zapLog)

		By("creating the gpu-mig-e2e namespace")
		_, err = clientset.CoreV1().Namespaces().Create(context.Background(), &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: gpuMigFixtureNamespace},
		}, metav1.CreateOptions{})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			Expect(err).NotTo(HaveOccurred())
		}

		By("deploying the MIG DCGM-exposition fixture pod")
		deployMigFixture(clientset)
	})

	AfterAll(func() {
		By("removing the gpu-mig-e2e namespace")
		_ = clientset.CoreV1().Namespaces().Delete(context.Background(), gpuMigFixtureNamespace, metav1.DeleteOptions{})
	})

	// The non-MIG case: a real run-ai/fake-gpu-operator status-exporter,
	// live on the cluster, reporting 2 distinct whole GPUs. This node was
	// never affected by the bug (every row already had a distinct
	// DeviceUUID), but it's the control case proving the fix didn't change
	// correct non-MIG behavior, scraped through the exact same production
	// code as the MIG case below.
	// GPUUtilizationAvg is asserted as an exact 0 below, not a bounded range
	// — this is deterministic, not a race that happens to read as 0 today.
	// run-ai/fake-gpu-operator's status-exporter derives DCGM_FI_DEV_GPU_UTIL
	// from summing Utilization() over the GPU's PodGpuUsageStatusMap (see
	// internal/common/topology/podGpuUsageStatusMap.go), keyed by pod UID.
	// No pod in this test requests a GPU from this node's simulated pool, so
	// that map is unconditionally empty and the sum is structurally 0 —
	// there's no randomized-range code path (topology/range.go's Random())
	// in play here at all, since it only executes per map entry.
	It("reports the physical GPU count and utilization average for a non-MIG node", func() {
		ctx := context.Background()

		By("finding the node run-ai/fake-gpu-operator labeled as the default (non-MIG) pool")
		nodeName := findNodeWithLabel(ctx, clientset, simulatedGpuNodePoolLabel, gpuPlainNodePool)

		var summary *nodemon.NodeGPUSummary
		Eventually(func() *nodemon.NodeGPUSummary {
			summary = queryNodeGPUSummary(ctx, clientset, dynClient, restConfig, log, nodeName, "app=nvidia-dcgm-exporter")
			return summary
		}, 2*time.Minute, 5*time.Second).ShouldNot(BeNil(),
			"expected the real nvidia-dcgm-exporter pod on this node to be scrapeable")

		Expect(summary.GPUCount).To(Equal(float64(2)))
		Expect(summary.GPUInstanceCount).To(Equal(float64(2)))
		Expect(summary.GPUUtilizationAvg).To(Equal(float64(0)),
			"both simulated Tesla-K80s are idle")
	})

	// The MIG case, reproducing the exact node shape found on the customer's
	// p4d.24xlarge node this bug was found on: 3 whole GPUs (2 claimed, 1
	// idle) plus 5 MIG-partitioned physical GPUs carrying 12x "1g.5gb" + 7x
	// "3g.20gb" instances — 8 physical GPUs, 22 DCGM rows, matching that
	// node's live NodePool status.resources exactly. Before the fix,
	// GPUCount counted DCGM rows (22) instead of physical GPUs (8), and
	// GPUUtilizationAvg averaged in all 19 MIG rows' unreported (zero)
	// utilization, diluting (90+15+0)/3=35 down to 105/22=4.77.
	It("reports the true physical GPU count and utilization average for an 8-physical-GPU / 22-DCGM-row MIG node", func() {
		ctx := context.Background()

		By("finding the node hosting the MIG DCGM-exposition fixture")
		nodeName := findNodeWithLabel(ctx, clientset, "zxporter-e2e/gpu-role", "mig-fixture")

		var summary *nodemon.NodeGPUSummary
		Eventually(func() *nodemon.NodeGPUSummary {
			summary = queryNodeGPUSummary(ctx, clientset, dynClient, restConfig, log, nodeName, "zxporter-e2e/fixture=mig")
			return summary
		}, 2*time.Minute, 5*time.Second).ShouldNot(BeNil(),
			"expected the MIG fixture pod on this node to be scrapeable")

		Expect(summary.GPUCount).To(Equal(float64(8)),
			"3 whole + 5 MIG-partitioned physical GPUs = 8 physical GPUs, not 22 DCGM rows")
		Expect(summary.GPUInstanceCount).To(Equal(float64(22)),
			"22 DCGM rows: 3 whole GPU rows + 19 MIG-instance rows (12x 1g.5gb + 7x 3g.20gb)")
		Expect(summary.GPUUtilizationAvg).To(Equal(float64(35)),
			"only the 3 whole GPUs report GPU_UTIL, (90+15+0)/3=35; averaging in the 19 unreported MIG rows gives 4.77")
		Expect(summary.GPUUtilizationMax).To(Equal(float64(90)))
		Expect(summary.GPUMemoryUsedTotal).To(Equal(float64(75198)),
			"framebuffer memory sums across all 22 rows — MIG slice memory is real physical memory")
		Expect(summary.GPUMigInstances).To(HaveLen(19),
			"19 MIG-instance rows: 12x 1g.5gb + 7x 3g.20gb, carried through with their MIG identity")
	})
})

// queryNodeGPUSummary drives the exact production entry point
// (nodemon.Exporter.QueryMetrics, the same code cmd/zxporter-nodemon/main.go
// wires up) against the real cluster: list pods matching labelSelector on
// nodeName, scrape their /metrics over real HTTP, map, and summarize.
// Returns nil (rather than failing) when the pod isn't scrapeable yet, so
// callers can wrap this in Eventually.
//
// The scrape itself goes through a kubectl-port-forward-style tunnel
// (client-go's tools/portforward, via ExporterConfig.DCGMHost) rather than
// a direct HTTP call to the pod's cluster-internal IP. In production,
// zxporter-nodemon always scrapes DCGM from the same node (hostNetwork,
// same overlay segment), so that direct path is always reachable there —
// but this test's Go binary runs on the CI runner, outside the cluster
// entirely, and GitHub Actions' kind runners don't route the runner's host
// network namespace into kind's pod CIDR (confirmed: a bare `curl` from the
// runner to a live pod IP times out after 2 minutes, every time, even
// though `kubectl` itself works fine — that traffic goes through the node
// containers' published Docker ports, a separate path). A port-forward
// tunnels through the API server instead, which is the same path `kubectl`
// already proves reachable, so it works regardless of CNI/runner-network
// topology. This does mean ExporterConfig's own label+node pod-discovery
// branch (getDCGMUrls's dynamic-client list, still exercised elsewhere)
// isn't exercised by this specific call — DCGMHost short-circuits past it
// — the scrape, HTTP mapping, and summarization logic under test (where
// the actual bug lived) still run for real.
func queryNodeGPUSummary(
	ctx context.Context,
	clientset kubernetes.Interface,
	dynClient dynamic.Interface,
	restConfig *rest.Config,
	log logr.Logger,
	nodeName, labelSelector string,
) *nodemon.NodeGPUSummary {
	pods, err := clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
		FieldSelector: "status.phase=Running,spec.nodeName=" + nodeName,
	})
	if err != nil || len(pods.Items) == 0 {
		return nil
	}
	pod := pods.Items[0]

	localPort, stopPortForward, err := portForwardPod(restConfig, clientset, pod.Namespace, pod.Name, 9400)
	if err != nil {
		log.Error(err, "port-forwarding to DCGM pod failed", "pod", pod.Name)
		return nil
	}
	defer stopPortForward()

	cfg := nodemon.ExporterConfig{
		DCGMHost:            "127.0.0.1",
		DCGMPort:            localPort,
		DCGMMetricsEndpoint: "/metrics",
	}
	scraper := nodemon.NewScraper(&http.Client{Timeout: 15 * time.Second}, log)
	mapper := nodemon.NewMapper(nodeName, nil, log)
	exporter := nodemon.NewExporter(cfg, dynClient, scraper, mapper, log)

	metrics, err := exporter.QueryMetrics(ctx)
	if err != nil || len(metrics) == 0 {
		return nil
	}
	return nodemon.SummarizeNodeGPU(metrics)
}

// portForwardPod opens a client-go port-forward tunnel (the same mechanism
// `kubectl port-forward` uses — an SPDY stream through the API server) from
// an OS-assigned local port to remotePort on the given pod, and returns
// once the tunnel is ready. The caller must call the returned stop func to
// tear it down.
func portForwardPod(
	restConfig *rest.Config,
	clientset kubernetes.Interface,
	namespace, podName string,
	remotePort int,
) (localPort int, stop func(), err error) {
	roundTripper, upgrader, err := spdy.RoundTripperFor(restConfig)
	if err != nil {
		return 0, nil, fmt.Errorf("building SPDY round tripper: %w", err)
	}

	reqURL := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(namespace).
		Name(podName).
		SubResource("portforward").
		URL()
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: roundTripper}, http.MethodPost, reqURL)

	stopCh := make(chan struct{}, 1)
	readyCh := make(chan struct{})
	fw, err := portforward.New(dialer, []string{fmt.Sprintf("0:%d", remotePort)}, stopCh, readyCh, io.Discard, io.Discard)
	if err != nil {
		return 0, nil, fmt.Errorf("creating port forwarder: %w", err)
	}

	forwardErrCh := make(chan error, 1)
	go func() { forwardErrCh <- fw.ForwardPorts() }()

	select {
	case <-readyCh:
	case err := <-forwardErrCh:
		return 0, nil, fmt.Errorf("port forward exited before becoming ready: %w", err)
	case <-time.After(30 * time.Second):
		close(stopCh)
		return 0, nil, fmt.Errorf("port forward to %s/%s did not become ready within 30s", namespace, podName)
	}

	ports, err := fw.GetPorts()
	if err != nil || len(ports) == 0 {
		close(stopCh)
		return 0, nil, fmt.Errorf("reading forwarded port: %w", err)
	}

	return int(ports[0].Local), func() { close(stopCh) }, nil
}

// findNodeWithLabel fails the spec immediately (not via Eventually) since a
// missing node-pool label means the CI action's cluster setup is broken, not
// a timing issue worth retrying.
func findNodeWithLabel(ctx context.Context, clientset kubernetes.Interface, key, value string) string {
	nodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{
		LabelSelector: key + "=" + value,
	})
	Expect(err).NotTo(HaveOccurred())
	Expect(nodes.Items).NotTo(BeEmpty(), "no node found with label %s=%s", key, value)
	return nodes.Items[0].Name
}

// deployMigFixture schedules a plain busybox httpd pod, serving
// migFixtureMetrics verbatim on :9400/metrics, onto a worker node distinct
// from the one run-ai/fake-gpu-operator was assigned (the CI action labels
// exactly one worker with simulatedGpuNodePoolLabel) — it labels that other
// node "zxporter-e2e/gpu-role=mig-fixture" for the spec's own node lookup.
// Deliberately NOT labeled app=nvidia-dcgm-exporter: that would make the
// real nvidia-dcgm-exporter DaemonSet controller adopt-and-delete it as an
// unwanted extra pod matching its own selector on a node it doesn't target.
func deployMigFixture(clientset kubernetes.Interface) {
	ctx := context.Background()

	nodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	Expect(err).NotTo(HaveOccurred())

	var fixtureNode string
	for _, n := range nodes.Items {
		if _, isControlPlane := n.Labels["node-role.kubernetes.io/control-plane"]; isControlPlane {
			continue
		}
		if n.Labels[simulatedGpuNodePoolLabel] != "" {
			continue // reserved for run-ai/fake-gpu-operator's real DaemonSet
		}
		fixtureNode = n.Name
		break
	}
	Expect(fixtureNode).NotTo(BeEmpty(), "expected a second worker node free for the MIG fixture pod")

	n, err := clientset.CoreV1().Nodes().Get(ctx, fixtureNode, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	if n.Labels == nil {
		n.Labels = map[string]string{}
	}
	n.Labels["zxporter-e2e/gpu-role"] = "mig-fixture"
	_, err = clientset.CoreV1().Nodes().Update(ctx, n, metav1.UpdateOptions{})
	Expect(err).NotTo(HaveOccurred())

	_, err = clientset.CoreV1().ConfigMaps(gpuMigFixtureNamespace).Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "mig-fixture-metrics", Namespace: gpuMigFixtureNamespace},
		Data:       map[string]string{"metrics": migFixtureMetrics},
	}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		Expect(err).NotTo(HaveOccurred())
	}

	_, err = clientset.CoreV1().Pods(gpuMigFixtureNamespace).Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mig-fixture",
			Namespace: gpuMigFixtureNamespace,
			Labels:    map[string]string{"zxporter-e2e/fixture": "mig"},
		},
		Spec: corev1.PodSpec{
			NodeSelector: map[string]string{"zxporter-e2e/gpu-role": "mig-fixture"},
			Tolerations:  []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
			Containers: []corev1.Container{{
				Name:    "dcgm-fixture",
				Image:   "busybox:1.36",
				Command: []string{"httpd", "-f", "-v", "-p", "9400", "-h", "/www"},
				Ports:   []corev1.ContainerPort{{ContainerPort: 9400}},
				VolumeMounts: []corev1.VolumeMount{
					{Name: "metrics", MountPath: "/www"},
				},
				ReadinessProbe: &corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{
						TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt(9400)},
					},
					InitialDelaySeconds: 2,
					PeriodSeconds:       2,
				},
			}},
			Volumes: []corev1.Volume{{
				Name: "metrics",
				VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: "mig-fixture-metrics"},
					},
				},
			}},
		},
	}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		Expect(err).NotTo(HaveOccurred())
	}

	By("waiting for the MIG fixture pod to become Ready")
	Eventually(func() bool {
		p, err := clientset.CoreV1().Pods(gpuMigFixtureNamespace).Get(ctx, "mig-fixture", metav1.GetOptions{})
		if err != nil {
			return false
		}
		for _, c := range p.Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
				return true
			}
		}
		return false
	}, 2*time.Minute, 2*time.Second).Should(BeTrue())
}
