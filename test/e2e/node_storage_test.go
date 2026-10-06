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
	"time"

	. "github.com/onsi/ginkgo/v2" //nolint:golint,revive
	. "github.com/onsi/gomega"    //nolint:golint,revive

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/devzero-inc/zxporter/internal/collector"
)

// Node storage occupancy, end to end against a real kubelet.
//
// Every other test of this feature reads a captured fixture. This one drives
// the production client against the live kubelet on a real node and asserts
// the invariants the whole feature rests on. Three things only a real node
// can prove:
//
//  1. The kubelet actually serves node.fs and node.runtime.imageFs at all,
//     through the API-server nodes/proxy subresource this client uses. A
//     fixture cannot catch the kubelet dropping a section, renaming a field,
//     or the RBAC verb going missing.
//  2. imagefs is a SUBSET of nodefs when they share a device. This is the
//     invariant the entire feature is built on -- get it wrong and consumers
//     double-count disk -- and it has only ever been asserted against numbers
//     we wrote ourselves.
//  3. The shared-device heuristic classifies a real node correctly. It is a
//     heuristic precisely because the kubelet does not name the device, so
//     the only honest test of it is a node whose real layout we know.
//
// Unlike the gpu-mig spec this deploys no fixture and needs no port-forward:
// the client reads through the API server's nodes/proxy subresource, the same
// path kubectl already proves reachable from the runner.
var _ = Describe("Node storage occupancy", Ordered, Label("node-storage"), func() {
	var (
		clientset kubernetes.Interface
		log       logr.Logger
		nodeNames []string
	)

	BeforeAll(func() {
		restConfig := ctrl.GetConfigOrDie()

		var err error
		clientset, err = kubernetes.NewForConfig(restConfig)
		Expect(err).NotTo(HaveOccurred())

		zapLog, _ := zap.NewDevelopment()
		log = zapr.NewLogger(zapLog)

		By("listing the cluster's nodes")
		nodes, err := clientset.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(nodes.Items).NotTo(BeEmpty(), "expected at least one node to read storage from")
		for _, n := range nodes.Items {
			nodeNames = append(nodeNames, n.Name)
		}
	})

	// The kubelet populates node.fs from statfs on the filesystem backing
	// /var/lib/kubelet. Any node that can run pods has one, so these bounds
	// hold on every node in any cluster -- they are invariants, not
	// Kind-specific values.
	It("reads a real kubelet's filesystem occupancy through the API-server proxy", func() {
		ctx := context.Background()
		client := collector.NewKubeletSummaryClient(clientset, log, 30*time.Second)

		for _, nodeName := range nodeNames {
			By("fetching node storage for " + nodeName)

			var m *collector.UnifiedNodeMetric
			Eventually(func() error {
				var err error
				m, err = client.FetchNodeStorageByNode(ctx, nodeName)
				return err
			}, 2*time.Minute, 5*time.Second).Should(Succeed(),
				"the kubelet Summary API should be reachable via nodes/proxy; a permission "+
					"error here means the nodes/proxy RBAC verb regressed")

			Expect(m).NotTo(BeNil())

			// A node that can schedule pods has a filesystem with a nonzero
			// size. Zero capacity is the sentinel the whole read path treats
			// as "we do not know", so a real node reporting it would mean the
			// section never arrived.
			Expect(m.NodefsCapacityBytes).To(BeNumerically(">", 0),
				"node %s reported zero nodefs capacity -- the kubelet's fs section did not arrive", nodeName)
			Expect(m.NodefsUsedBytes).To(BeNumerically(">", 0),
				"node %s has a filesystem holding at least the kubelet's own state", nodeName)

			// used + available is normally strictly LESS than capacity:
			// ext4/xfs reserve blocks for root that belong to neither. The
			// assertion is therefore one-sided -- an implementation that
			// crossed two fields would break it, a correct one cannot.
			Expect(m.NodefsUsedBytes+m.NodefsAvailableBytes).To(BeNumerically("<=", m.NodefsCapacityBytes),
				"node %s: used+available must not exceed capacity", nodeName)

			Expect(m.NodefsInodesTotal).To(BeNumerically(">", 0), "node %s", nodeName)
			Expect(m.NodefsInodesUsed).To(BeNumerically(">", 0), "node %s", nodeName)
			Expect(m.NodefsInodesUsed).To(BeNumerically("<=", m.NodefsInodesTotal),
				"node %s: inodes used cannot exceed inodes total", nodeName)
		}
	})

	// The invariant the feature is built on. Whether a given node shares the
	// device is an environment fact, so this asserts the CONSEQUENCE of the
	// flag rather than its value -- it holds on a split-disk node too.
	It("never reports imagefs usage exceeding nodefs usage on a shared device", func() {
		ctx := context.Background()
		client := collector.NewKubeletSummaryClient(clientset, log, 30*time.Second)

		sharedSeen := false

		for _, nodeName := range nodeNames {
			m, err := client.FetchNodeStorageByNode(ctx, nodeName)
			Expect(err).NotTo(HaveOccurred())
			Expect(m).NotTo(BeNil())

			if m.ImagefsCapacityBytes == 0 {
				// A runtime that reports no imagefs section at all. Nothing to
				// check, and nothing downstream will read it: the read paths
				// gate on a nonzero capacity.
				continue
			}

			if !m.ImagefsSharedDevice {
				// A genuinely separate image filesystem. The two are then
				// independent and no subset relationship is implied.
				continue
			}

			sharedSeen = true

			// This is the whole point. When the flag says shared, imagefs
			// usage is image layers sitting INSIDE the nodefs total -- adding
			// the two double-counts. If this ever fails, either the heuristic
			// misclassified a split-disk node or the kubelet's two sections
			// stopped describing the same filesystem, and every consumer that
			// trusts the flag is now wrong.
			Expect(m.ImagefsUsedBytes).To(BeNumerically("<=", m.NodefsUsedBytes),
				"node %s: imagefs is flagged as sharing a device with nodefs, so its used bytes "+
					"must be a subset of nodefs used bytes, not an addition to it", nodeName)

			// The heuristic's own inputs, restated as an assertion so a
			// failure says which half diverged rather than just "not shared".
			Expect(m.ImagefsCapacityBytes).To(Equal(m.NodefsCapacityBytes),
				"node %s: shared-device classification requires equal capacity", nodeName)
			Expect(m.NodefsInodesTotal).To(BeNumerically(">", 0),
				"node %s: shared-device classification requires a nonzero inode total", nodeName)
		}

		// Kind runs containerd with its state on the same filesystem as the
		// kubelet's, which is also the default on stock EKS AL2023, GKE COS
		// and AKS Ubuntu. If no node classifies as shared, the heuristic has
		// almost certainly stopped matching real kubelet output -- which is
		// exactly the regression a fixture-based test cannot see.
		Expect(sharedSeen).To(BeTrue(),
			"expected at least one node to report imagefs sharing a device with nodefs; "+
				"on this cluster's runtime they are the same filesystem, so a universal "+
				"'not shared' means the heuristic no longer matches real kubelet output")
	})

	// Ephemeral storage comes from the Node object rather than the kubelet,
	// so it is populated even for a node no metrics source ever reached. This
	// asserts the two sources agree about roughly the same disk -- allocatable
	// is capacity minus reservations, so it is bounded by it.
	It("reports ephemeral-storage capacity and allocatable from the Node object", func() {
		ctx := context.Background()

		for _, nodeName := range nodeNames {
			node, err := clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			capacity := node.Status.Capacity[corev1.ResourceEphemeralStorage]
			allocatable := node.Status.Allocatable[corev1.ResourceEphemeralStorage]

			Expect(capacity.Value()).To(BeNumerically(">", 0),
				"node %s reports no ephemeral-storage capacity; the scheduler admits pods "+
					"against this value, so a zero here would break the committed-vs-available view", nodeName)
			Expect(allocatable.Value()).To(BeNumerically(">", 0), "node %s", nodeName)
			Expect(allocatable.Value()).To(BeNumerically("<=", capacity.Value()),
				"node %s: allocatable is capacity minus reservations", nodeName)
		}
	})
})
