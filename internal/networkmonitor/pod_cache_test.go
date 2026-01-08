package networkmonitor

import (
	"context"
	"testing"
	"time"

	"inet.af/netaddr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
)

func TestPodCache_UpdateLogic(t *testing.T) {
	// 1. Setup Fake Client and Informer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client := fake.NewSimpleClientset()
	factory := informers.NewSharedInformerFactory(client, 0)
	informer := factory.Core().V1().Pods().Informer()

	pc := NewPodCache(informer)

	// Start informer factory
	factory.Start(ctx.Done())
	synced := factory.WaitForCacheSync(ctx.Done())
	for kind, ok := range synced {
		if !ok {
			t.Fatalf("cache %v failed to sync", kind)
		}
	}

	// 2. Add a Pod
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
		},
		Status: corev1.PodStatus{
			PodIP: "10.0.0.1",
		},
	}
	_, err := client.CoreV1().Pods("default").Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Failed to create pod: %v", err)
	}

	// Wait for cache to sync (poll)
	ip1 := netaddr.MustParseIP("10.0.0.1")
	waitForPod(t, pc, ip1, "test-pod")

	// 3. Update the Pod with a NEW IP
	newPod := pod.DeepCopy()
	newPod.Status.PodIP = "10.0.0.2"
	_, err = client.CoreV1().Pods("default").Update(ctx, newPod, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("Failed to update pod: %v", err)
	}

	// 4. Verify New IP works
	ip2 := netaddr.MustParseIP("10.0.0.2")
	waitForPod(t, pc, ip2, "test-pod")

	// 5. Verify Old IP is missing (Stale entry removal)
	// Give a small window for the event to process if not already caught by waitForPod
	// but since waitForPod succeeded for ip2, the update handler must have run.
	// The update handler removes ip1 BEFORE adding ip2, or in the same execution.
	// So checking immediately should be safe.
	if p, ok := pc.GetPodByIP(ip1); ok {
		t.Errorf("Stale IP %s still exists in cache, mapped to %s", ip1, p.Name)
	}

	// 6. Delete the Pod
	err = client.CoreV1().Pods("default").Delete(ctx, "test-pod", metav1.DeleteOptions{})
	if err != nil {
		t.Fatalf("Failed to delete pod: %v", err)
	}

	// 7. Verify deletion
	waitForPodDeletion(t, pc, ip2)
}

func waitForPod(t *testing.T, pc *PodCache, ip netaddr.IP, expectedName string) {
	t.Helper()
	timeout := time.After(2 * time.Second)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			t.Fatalf("Timeout waiting for pod with IP %s", ip)
		case <-ticker.C:
			if pod, ok := pc.GetPodByIP(ip); ok {
				if pod.Name == expectedName {
					return // Found and correct
				}
			}
		}
	}
}

func waitForPodDeletion(t *testing.T, pc *PodCache, ip netaddr.IP) {
	t.Helper()
	timeout := time.After(2 * time.Second)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			t.Fatalf("Timeout waiting for pod deletion IP %s", ip)
		case <-ticker.C:
			if _, ok := pc.GetPodByIP(ip); !ok {
				return // Deleted
			}
		}
	}
}
