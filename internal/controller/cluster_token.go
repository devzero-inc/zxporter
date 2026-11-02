package controller

import (
	"context"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/devzero-inc/zxporter/internal/transport"
)

// clusterTokenSourceMirror is the CLUSTER_TOKEN_SOURCE value the chart and
// the kubectl manifests set when CLUSTER_TOKEN is fed from the mirror Secret
// devzero-zxporter-token, which zxporter itself writes (KUB-158).
const clusterTokenSourceMirror = "mirror"

// envTokenIsMirrorFed reports whether the env token is only the pod-start
// snapshot of the mirror Secret. The polarity is fail-safe: only an explicit
// mirror marker in Secret mode counts. An unset marker (an older chart), an
// existingSecret, a direct clusterToken and ConfigMap mode all keep the env
// token authoritative.
func envTokenIsMirrorFed(useSecret bool, source string) bool {
	return useSecret && source == clusterTokenSourceMirror
}

// clusterTokenResolver decides which cluster token this zxporter runs with.
// It only reads: the caller persists the result to the mirror Secret or
// ConfigMap.
type clusterTokenResolver struct {
	k8s kubernetes.Interface
	// mirrorFed says the env token is the pod-start snapshot of the mirror
	// Secret that readStored reads live (see envTokenIsMirrorFed).
	mirrorFed  bool
	readStored func(ctx context.Context) string
	exchange   func(ctx context.Context, req transport.ClusterTokenExchangeRequest) (transport.ClusterTokenExchangeResult, error)
	log        logr.Logger
}

// resolve picks the cluster token.
//
// When the env token is mirror-fed, the live mirror wins and the env snapshot
// is only a fallback: a standby promoted to leader after another replica
// re-exchanged must not run with, and write back, its older token. Otherwise
// the env token is explicit and wins, and the stored token is the fallback.
//
// Without either, a PAT is exchanged for a new token. The exchange carries
// the kube-system namespace UID, so dakr re-attaches the same cluster, and
// the trimmed CLUSTER_ID. A CLUSTER_ID that dakr does not know fails startup
// rather than registering a new cluster.
func (r *clusterTokenResolver) resolve(ctx context.Context, envToken, pat, clusterName, provider, clusterID string) (string, error) {
	var token, source string
	if r.mirrorFed {
		token, source = r.readStored(ctx), "live mirror Secret"
		if token == "" {
			token, source = envToken, "env"
		}
	} else {
		token, source = envToken, "env"
		if token == "" {
			token, source = r.readStored(ctx), "stored"
		}
	}
	if token != "" {
		r.log.Info("Using cluster token", "source", source)
		return token, nil
	}
	if pat == "" {
		return "", nil
	}

	clusterID = strings.TrimSpace(clusterID)
	res, err := r.exchange(ctx, transport.ClusterTokenExchangeRequest{
		PATToken: pat, ClusterName: clusterName, K8sProvider: provider,
		KubeSystemUID: r.kubeSystemUID(ctx), ClusterID: clusterID,
	})
	if err != nil {
		if clusterID != "" && connect.CodeOf(err) == connect.CodeNotFound {
			return "", fmt.Errorf("cluster %s named by CLUSTER_ID is not a live cluster of this token's team; refusing to register a new one: %w", clusterID, err)
		}
		return "", err
	}
	r.log.Info("Using cluster token", "source", "exchange", "clusterId", res.ClusterID, "reattached", res.Reattached)
	return res.Token, nil
}

func (r *clusterTokenResolver) kubeSystemUID(ctx context.Context) string {
	ns, err := r.k8s.CoreV1().Namespaces().Get(ctx, "kube-system", metav1.GetOptions{})
	if err != nil {
		r.log.Info("Could not read kube-system namespace; exchanging without a cluster fingerprint", "error", err.Error())
		return ""
	}
	return string(ns.UID)
}
