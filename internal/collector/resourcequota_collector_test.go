package collector

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// resourceQuotaFixture builds a ResourceQuota whose only interesting fields are
// the ones the change detector inspects.
func resourceQuotaFixture(resourceVersion string, used string) *corev1.ResourceQuota {
	return &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "compute-quota",
			Namespace:       "team-a",
			ResourceVersion: resourceVersion,
			Labels:          map[string]string{"app": "api"},
		},
		Spec: corev1.ResourceQuotaSpec{
			Hard: corev1.ResourceList{
				corev1.ResourceLimitsCPU: resource.MustParse("10"),
			},
		},
		Status: corev1.ResourceQuotaStatus{
			Hard: corev1.ResourceList{
				corev1.ResourceLimitsCPU: resource.MustParse("10"),
			},
			Used: corev1.ResourceList{
				corev1.ResourceLimitsCPU: resource.MustParse(used),
			},
		},
	}
}

func TestResourceQuotaChanged(t *testing.T) {
	c := &ResourceQuotaCollector{}

	tests := []struct {
		name string
		old  *corev1.ResourceQuota
		new  func() *corev1.ResourceQuota
		want bool
	}{
		{
			// The bug this guards: a status-only update bumps resourceVersion
			// but touches neither labels nor annotations, so the ObjectMeta
			// check returns UnknownChanges and the status comparison below it
			// must still run.
			name: "status.used changed with a bumped resourceVersion",
			old:  resourceQuotaFixture("100", "4"),
			new:  func() *corev1.ResourceQuota { return resourceQuotaFixture("101", "7") },
			want: true,
		},
		{
			name: "only resourceVersion changed",
			old:  resourceQuotaFixture("100", "4"),
			new:  func() *corev1.ResourceQuota { return resourceQuotaFixture("101", "4") },
			want: false,
		},
		{
			name: "label changed",
			old:  resourceQuotaFixture("100", "4"),
			new: func() *corev1.ResourceQuota {
				rq := resourceQuotaFixture("101", "4")
				rq.Labels = map[string]string{"app": "worker"}
				return rq
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := c.resourceQuotaChanged(tt.old, tt.new()); got != tt.want {
				t.Errorf("resourceQuotaChanged() = %v, want %v", got, tt.want)
			}
		})
	}
}
