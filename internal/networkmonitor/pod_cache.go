package networkmonitor

import (
	"sync"

	"inet.af/netaddr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/cache"
)

// PodCache watches for pods on the local node and maintains an IP lookup table
type PodCache struct {
	mu    sync.RWMutex
	ips   map[netaddr.IP]*corev1.Pod // Using netaddr.IP for efficient lookup
	store cache.Store
}

// NewPodCache creates a new PodCache using the provided informer.
// If informer is nil, it returns a cache that relies only on manual updates (standalone mode).
func NewPodCache(informer cache.SharedIndexInformer) *PodCache {
	pc := &PodCache{
		ips: make(map[netaddr.IP]*corev1.Pod),
	}

	if informer != nil {
		informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				pc.updatePod(obj.(*corev1.Pod))
			},
			UpdateFunc: func(old, new interface{}) {
				pc.updatePod(new.(*corev1.Pod))
			},
			DeleteFunc: func(obj interface{}) {
				pc.deletePod(obj)
			},
		})
		pc.store = informer.GetStore()
	}
	return pc
}

func (pc *PodCache) updatePod(pod *corev1.Pod) {
	if pod.Status.PodIP == "" {
		return
	}

	ip, err := netaddr.ParseIP(pod.Status.PodIP)
	if err != nil {
		return
	}

	pc.mu.Lock()
	defer pc.mu.Unlock()
	pc.ips[ip] = pod
}

func (pc *PodCache) deletePod(obj interface{}) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		// handle DeletedFinalStateUnknown
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			return
		}
		pod, ok = tombstone.Obj.(*corev1.Pod)
		if !ok {
			return
		}
	}

	if pod.Status.PodIP == "" {
		return
	}

	ip, err := netaddr.ParseIP(pod.Status.PodIP)
	if err != nil {
		return
	}

	pc.mu.Lock()
	defer pc.mu.Unlock()
	delete(pc.ips, ip)
}

// GetPodByIP returns the pod for a given IP if it exists on the local node
func (pc *PodCache) GetPodByIP(ip netaddr.IP) (*corev1.Pod, bool) {
	pc.mu.RLock()
	defer pc.mu.RUnlock()
	pod, ok := pc.ips[ip]
	return pod, ok
}

// GetLocalPodIPs returns a map of all local pod IPs
func (pc *PodCache) GetLocalPodIPs() map[netaddr.IP]struct{} {
	pc.mu.RLock()
	defer pc.mu.RUnlock()

	res := make(map[netaddr.IP]struct{}, len(pc.ips))
	for ip := range pc.ips {
		res[ip] = struct{}{}
	}
	return res
}
