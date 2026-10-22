package collector

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// deploymentFixture builds a Deployment whose only interesting fields are the
// ones the change detector inspects.
func deploymentFixture(resourceVersion string, updatedReplicas int32) *appsv1.Deployment {
	replicas := int32(3)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "api",
			Namespace:       "team-a",
			ResourceVersion: resourceVersion,
			Generation:      1,
			Labels:          map[string]string{"app": "api"},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
		},
		Status: appsv1.DeploymentStatus{
			Replicas:           3,
			ReadyReplicas:      3,
			AvailableReplicas:  3,
			UpdatedReplicas:    updatedReplicas,
			ObservedGeneration: 1,
		},
	}
}

func TestDeploymentChanged(t *testing.T) {
	c := &DeploymentCollector{}

	tests := []struct {
		name string
		old  *appsv1.Deployment
		new  *appsv1.Deployment
		want bool
	}{
		{
			// Rollout progress: only status moves, so the ObjectMeta check
			// returns UnknownChanges and the status comparison below it must
			// still run.
			name: "status.updatedReplicas changed with a bumped resourceVersion",
			old:  deploymentFixture("100", 1),
			new:  deploymentFixture("101", 3),
			want: true,
		},
		{
			name: "only resourceVersion changed",
			old:  deploymentFixture("100", 3),
			new:  deploymentFixture("101", 3),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := c.deploymentChanged(tt.old, tt.new); got != tt.want {
				t.Errorf("deploymentChanged() = %v, want %v", got, tt.want)
			}
		})
	}
}
