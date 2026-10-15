package redmetrics

import (
	corev1 "k8s.io/api/core/v1"
)

// ContainerKey identifies the container a measurement belongs to, in the only
// terms this agent can see. The DevZero container, pod and node UUIDs are
// resolved server side from exactly these fields, by the same lookup the existing
// bucketed container metrics path uses -- which is what makes the two tables agree
// about which container a measurement belongs to.
type ContainerKey struct {
	PodName       string
	PodNamespace  string
	PodUID        string
	ContainerName string
}

// containerForPort names the container serving a port on a pod, reporting false
// when it cannot be established.
//
// Port is the discriminator because of a physical fact rather than a convention:
// the containers of a pod share one network namespace, so no two of them can bind
// the same port. What the pod spec declares is a different matter -- containerPort
// is advisory in Kubernetes and a container that never declares one still listens
// perfectly well -- so a declaration is treated as evidence, not as truth.
//
// The rules, in order:
//
//   - Exactly one candidate declares the port: that container.
//   - Several declare it: refuse. The declarations cannot all be true and there is
//     nothing here to choose between them.
//   - Nobody declares it and there is exactly one candidate: that container.
//     Whoever is listening, it is the only one that can be.
//   - Nobody declares it and there are several candidates: refuse.
//
// Refusing loses the measurement. That is the intended trade: a guess would file
// one workload's traffic against another workload's resource recommendations, and
// a wrong join is worse than a missing one.
func containerForPort(pod *corev1.Pod, port uint16) (string, bool) {
	if pod == nil {
		return "", false
	}

	var (
		declared string
		matches  int
		single   string
		count    int
	)

	visit := func(c *corev1.Container) {
		count++
		single = c.Name
		for i := range c.Ports {
			if uint16(c.Ports[i].ContainerPort) == port { //nolint:gosec // a port never exceeds 65535
				matches++
				declared = c.Name
				break
			}
		}
	}

	for i := range pod.Spec.Containers {
		visit(&pod.Spec.Containers[i])
	}
	// Native sidecars are init containers that keep running, so they can hold a
	// listening socket for as long as the pod is up and belong in the candidate set.
	// An ordinary init container has exited by the time any of this is observed, and
	// counting it would break the single-container fallback for the common case of
	// one app container behind one init container.
	for i := range pod.Spec.InitContainers {
		if isRestartableSidecar(&pod.Spec.InitContainers[i]) {
			visit(&pod.Spec.InitContainers[i])
		}
	}

	if matches == 1 {
		return declared, true
	}
	if matches > 1 {
		return "", false
	}
	if count == 1 {
		return single, true
	}
	return "", false
}

// isRestartableSidecar reports whether an init container is a native sidecar, the
// only kind that is still running when traffic arrives.
func isRestartableSidecar(c *corev1.Container) bool {
	return c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways
}
