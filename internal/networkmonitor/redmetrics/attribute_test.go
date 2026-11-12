package redmetrics

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func container(name string, ports ...int32) corev1.Container {
	c := corev1.Container{Name: name}
	for _, p := range ports {
		c.Ports = append(c.Ports, corev1.ContainerPort{ContainerPort: p})
	}
	return c
}

func sidecar(name string, ports ...int32) corev1.Container {
	c := container(name, ports...)
	always := corev1.ContainerRestartPolicyAlways
	c.RestartPolicy = &always
	return c
}

func pod(containers []corev1.Container, initContainers []corev1.Container) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"},
		Spec: corev1.PodSpec{
			Containers:     containers,
			InitContainers: initContainers,
		},
	}
}

// TestContainerForPort covers the whole attribution decision, including every case
// where it refuses.
//
// The refusals matter more than the successes. Attribution feeds a join against
// resource recommendations, so a wrong answer files one workload's traffic against
// another workload's requests and limits -- a plausible-looking number that is
// simply about a different container. A missing answer is visible as a gap; a wrong
// one is not visible at all.
func TestContainerForPort(t *testing.T) {
	tests := []struct {
		name string
		pod  *corev1.Pod
		port uint16
		want string
		ok   bool
	}{
		{
			name: "single container, port undeclared: whoever is listening, it is the only one that can be",
			pod:  pod([]corev1.Container{container("app")}, nil),
			port: 8080,
			want: "app",
			ok:   true,
		},
		{
			name: "single container declaring a different port still gets the traffic",
			pod:  pod([]corev1.Container{container("app", 9090)}, nil),
			port: 8080,
			want: "app",
			ok:   true,
		},
		{
			name: "two containers, one declares the port",
			pod:  pod([]corev1.Container{container("app", 8080), container("proxy", 15001)}, nil),
			port: 8080,
			want: "app",
			ok:   true,
		},
		{
			name: "two containers, neither declares the port: refuse rather than guess",
			pod:  pod([]corev1.Container{container("app"), container("proxy")}, nil),
			port: 8080,
			ok:   false,
		},
		{
			name: "two containers declare the same port: the spec is not self-consistent, refuse",
			pod:  pod([]corev1.Container{container("app", 8080), container("proxy", 8080)}, nil),
			port: 8080,
			ok:   false,
		},
		{
			name: "native sidecar declares the port",
			pod:  pod([]corev1.Container{container("app", 8080)}, []corev1.Container{sidecar("proxy", 15001)}),
			port: 15001,
			want: "proxy",
			ok:   true,
		},
		{
			name: "native sidecar is a candidate, so an undeclared port is now ambiguous",
			pod:  pod([]corev1.Container{container("app")}, []corev1.Container{sidecar("proxy")}),
			port: 8080,
			ok:   false,
		},
		{
			// An ordinary init container has exited long before any traffic arrives.
			// Counting it would break the single-container fallback for the very
			// common one-app-container-behind-one-init-container pod.
			name: "ordinary init container is not a candidate",
			pod:  pod([]corev1.Container{container("app")}, []corev1.Container{container("migrate")}),
			port: 8080,
			want: "app",
			ok:   true,
		},
		{
			name: "no pod",
			pod:  nil,
			port: 8080,
			ok:   false,
		},
		{
			name: "no containers at all",
			pod:  pod(nil, nil),
			port: 8080,
			ok:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := containerForPort(tt.pod, tt.port)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v (container %q)", ok, tt.ok, got)
			}
			if ok && got != tt.want {
				t.Fatalf("container = %q, want %q", got, tt.want)
			}
		})
	}
}
