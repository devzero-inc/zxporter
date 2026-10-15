package controller

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/devzero-inc/zxporter/internal/transport"
)

// exchangeCountingClient is a DakrClient whose PAT exchange returns a fixed
// token and counts the calls.
type exchangeCountingClient struct {
	transport.DakrClient
	exchanges *int
}

func (c exchangeCountingClient) ExchangePATForClusterToken(
	context.Context, transport.ClusterTokenExchangeRequest,
) (transport.ClusterTokenExchangeResult, error) {
	*c.exchanges++
	return transport.ClusterTokenExchangeResult{Token: "dakr-exchanged", ClusterID: "c-1"}, nil
}

// In ConfigMap mode a PAT install persists the exchanged token into the
// env ConfigMap, so the next start reads it back instead of exchanging (and,
// since KUB-158, rotating) the token again.
func TestInitializeTelemetryComponentsPersistsToConfigMapMode(t *testing.T) {
	const ns, cmName = "devzero-system", "devzero-zxporter-env-config"
	t.Setenv("CLUSTER_TOKEN", "")
	t.Setenv("PAT_TOKEN", "dzu-pat")
	t.Setenv("DAKR_URL", "")
	t.Setenv("CLUSTER_ID", "")
	t.Setenv("CLUSTER_TOKEN_SOURCE", "")
	t.Setenv("USE_SECRET_FOR_TOKEN", "false")
	t.Setenv("TOKEN_CONFIGMAP_NAME", "")
	t.Setenv("POD_NAMESPACE", ns)

	k := fake.NewSimpleClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: types.UID("ks-uid")}},
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: cmName, Namespace: ns},
			Data:       map[string]string{"CLUSTER_TOKEN": "", "OTHER": "kept"},
		},
	)
	exchanges := 0
	newController := func() *EnvBasedController {
		return &EnvBasedController{
			Log:        logr.Discard(),
			K8sClient:  k,
			Reconciler: &CollectionPolicyReconciler{ZapLogger: zap.NewNop()},
			dakrClientFactory: func(string, string, logr.Logger) transport.DakrClient {
				return exchangeCountingClient{DakrClient: transport.NewSimpleDakrClient(logr.Discard()), exchanges: &exchanges}
			},
		}
	}
	initialize := func() {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		c := newController()
		if err := c.initializeTelemetryComponents(ctx); err != nil {
			t.Fatalf("initializeTelemetryComponents: %v", err)
		}
		c.Reconciler.TelemetryLogger.Stop()
	}

	initialize()
	cm, err := k.CoreV1().ConfigMaps(ns).Get(context.Background(), cmName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get ConfigMap: %v", err)
	}
	if cm.Data["CLUSTER_TOKEN"] != "dakr-exchanged" || cm.Data["OTHER"] != "kept" {
		t.Fatalf("ConfigMap after the first start: token persisted=%t, other keys kept=%t",
			cm.Data["CLUSTER_TOKEN"] == "dakr-exchanged", cm.Data["OTHER"] == "kept")
	}
	if exchanges != 1 {
		t.Fatalf("first start made %d exchanges, want 1", exchanges)
	}

	initialize()
	if exchanges != 1 {
		t.Fatalf("second start exchanged again (%d exchanges in total); want it to read the stored token", exchanges)
	}
	if s, _ := k.CoreV1().Secrets(ns).List(context.Background(), metav1.ListOptions{}); len(s.Items) != 0 {
		t.Fatalf("ConfigMap mode wrote %d Secrets", len(s.Items))
	}
}
