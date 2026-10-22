/*
Copyright 2026.

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
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/devzero-inc/zxporter/internal/collector"
)

// The runtimeclass suite drives the real RuntimeClassCollector in-process
// against a plain kind cluster -- no add-ons needed, RuntimeClass is a
// built-in node.k8s.io/v1 kind -- mirroring policy-collectors' and
// node-storage's split between infra setup (shell) and test logic (Go).
// The collector under test is exactly what the operator wires up in its
// registration table; the assertions read its public GetResourceChannel
// output: real informers, real API server, real watch events.

var _ = Describe("RuntimeClass collector", Ordered, Label("runtimeclass"), func() {
	var (
		clientset kubernetes.Interface
	)

	const (
		rcName     = "e2e-runtimeclass"
		rcNameDead = "e2e-runtimeclass-2"
	)

	BeforeAll(func() {
		cfg := ctrl.GetConfigOrDie()
		var err error
		clientset, err = kubernetes.NewForConfig(cfg)
		Expect(err).NotTo(HaveOccurred())
	})

	// The collector is created per-spec (not in BeforeAll) so each spec sees
	// only its own traffic: the informer replays every existing object on
	// Start, and a shared collector would re-emit the previous spec's objects.
	AfterAll(func() {
		_ = clientset.NodeV1().RuntimeClasses().Delete(
			context.Background(), rcName, metav1.DeleteOptions{})
		_ = clientset.NodeV1().RuntimeClasses().Delete(
			context.Background(), rcNameDead, metav1.DeleteOptions{})
	})

	It("collects add, update and delete through a real watch", func() {
		ctx := context.Background()
		c := collector.NewRuntimeClassCollector(
			clientset, nil, policyBatchSize, policyBatchTime, logr.Discard(), nil)
		Expect(c.Start(ctx)).To(Succeed())
		// RuntimeClass is a built-in kind: no CRD-absent skip path exists.
		Expect(c.IsAvailable(ctx)).To(BeTrue())

		sink := newCollectedSink(c.GetResourceChannel())
		DeferCleanup(func() { _ = c.Stop() })

		By("creating a RuntimeClass with a handler and overhead")
		_, err := clientset.NodeV1().RuntimeClasses().Create(ctx, &nodev1.RuntimeClass{
			ObjectMeta: metav1.ObjectMeta{Name: rcName},
			Handler:    "runc",
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		Eventually(func() bool {
			res, found := sink.find(func(r collector.CollectedResource) bool {
				return r.ResourceType == collector.RuntimeClass &&
					r.Key == rcName &&
					r.EventType == collector.EventTypeAdd
			})
			if !found {
				return false
			}
			rc, ok := res.Object.(*nodev1.RuntimeClass)
			if !ok {
				return false
			}
			return rc.Handler == "runc"
		}, 2*time.Minute, 2*time.Second).Should(BeTrue(),
			"the created RuntimeClass should be collected as an add with its typed object")

		By("updating the overhead and scheduling")
		// spec.handler is immutable on node.k8s.io/v1, so a real update
		// mutates overhead and scheduling -- the payload fields the collector
		// must re-detect -- while the handler stays what it was created with.
		_, err = clientset.NodeV1().RuntimeClasses().Update(ctx, &nodev1.RuntimeClass{
			ObjectMeta: metav1.ObjectMeta{
				Name:            rcName,
				ResourceVersion: resourceVersionOf(ctx, clientset, rcName),
			},
			Handler: "runc",
			Overhead: &nodev1.Overhead{
				PodFixed: corev1.ResourceList{"memory": resource.MustParse("1Gi")},
			},
			Scheduling: &nodev1.Scheduling{
				NodeSelector: map[string]string{"e2e.example.com/pool": "runtime"},
			},
		}, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())

		Eventually(func() bool {
			res, found := sink.find(func(r collector.CollectedResource) bool {
				return r.ResourceType == collector.RuntimeClass &&
					r.Key == rcName &&
					r.EventType == collector.EventTypeUpdate
			})
			if !found {
				return false
			}
			rc, ok := res.Object.(*nodev1.RuntimeClass)
			if !ok {
				return false
			}
			return rc.Handler == "runc" &&
				rc.Overhead != nil &&
				rc.Overhead.PodFixed.Memory().String() == "1Gi" &&
				rc.Scheduling != nil &&
				rc.Scheduling.NodeSelector["e2e.example.com/pool"] == "runtime"
		}, 2*time.Minute, 2*time.Second).Should(BeTrue(),
			"an overhead/scheduling change must re-collect as an update carrying the typed object")

		By("deleting the RuntimeClass")
		err = clientset.NodeV1().RuntimeClasses().Delete(ctx, rcName, metav1.DeleteOptions{})
		Expect(err).NotTo(HaveOccurred())

		Eventually(func() bool {
			res, found := sink.find(func(r collector.CollectedResource) bool {
				return r.ResourceType == collector.RuntimeClass &&
					r.Key == rcName &&
					r.EventType == collector.EventTypeDelete
			})
			if !found {
				return false
			}
			// The delete path must carry the uid so dakr can soft-delete the
			// right row: cluster-scoped, so Key is the bare name.
			rc, ok := res.Object.(*nodev1.RuntimeClass)
			return ok && string(rc.UID) != ""
		}, 2*time.Minute, 2*time.Second).Should(BeTrue(),
			"the delete must be collected with the object's uid")

		By("recreating a class, then stopping the collector")
		_, err = clientset.NodeV1().RuntimeClasses().Create(ctx, &nodev1.RuntimeClass{
			ObjectMeta: metav1.ObjectMeta{Name: rcNameDead},
			Handler:    "runc",
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		Eventually(func() bool {
			_, found := sink.find(func(r collector.CollectedResource) bool {
				return r.ResourceType == collector.RuntimeClass && r.Key == rcNameDead
			})
			return found
		}, 2*time.Minute, 2*time.Second).Should(BeTrue())

		Expect(c.Stop()).To(Succeed())
		// Stop is idempotent and safe after a second call.
		Expect(c.Stop()).To(Succeed())

		By("an excluded class is never collected")
		excluded := collector.NewRuntimeClassCollector(
			clientset, []string{rcNameDead}, policyBatchSize, policyBatchTime, logr.Discard(), nil)
		Expect(excluded.Start(ctx)).To(Succeed())
		excludedSink := newCollectedSink(excluded.GetResourceChannel())
		DeferCleanup(func() { _ = excluded.Stop() })

		Consistently(func() bool {
			_, found := excludedSink.find(func(r collector.CollectedResource) bool {
				return r.ResourceType == collector.RuntimeClass && r.Key == rcNameDead
			})
			return found
		}, 3*time.Second, 500*time.Millisecond).Should(BeFalse(),
			"a CollectionPolicy-excluded RuntimeClass must never reach the channel")
	})
})

// resourceVersionOf reads the current resourceVersion so an update lands
// instead of conflicting.
func resourceVersionOf(ctx context.Context, clientset kubernetes.Interface, name string) string {
	rc, err := clientset.NodeV1().RuntimeClasses().Get(ctx, name, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	return rc.ResourceVersion
}
