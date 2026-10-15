package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/devzero-inc/zxporter/internal/transport"
)

func TestResolveClusterToken(t *testing.T) {
	notFound := connect.NewError(connect.CodeNotFound, errors.New("resource not found"))
	cases := []struct {
		name          string
		mirrorFed     bool
		env           string
		stored        string // the live mirror Secret or ConfigMap token
		pat           string
		clusterID     string
		noKubeSystem  bool
		exchangeErr   error
		want          string
		wantErr       string // substring of the expected error
		wantExchanges int
		wantUID       string
		wantSentID    string
	}{
		{name: "env token is used", env: "dakr-A", stored: "dakr-B", pat: "dzu-pat", want: "dakr-A"},
		{name: "stored token when env is empty", stored: "dakr-B", pat: "dzu-pat", want: "dakr-B"},
		{
			name: "exchange carries the kube-system UID and the trimmed CLUSTER_ID", pat: "dzu-pat", clusterID: "  c-7\n",
			want: "dakr-exchanged", wantExchanges: 1, wantUID: "ks-uid", wantSentID: "c-7",
		},
		{
			name: "exchange without a UID when kube-system is unreadable", pat: "dzu-pat", noKubeSystem: true,
			want: "dakr-exchanged", wantExchanges: 1,
		},
		{
			name: "unknown CLUSTER_ID fails startup", pat: "dzu-pat", clusterID: "c-9", exchangeErr: notFound,
			wantErr: "c-9", wantExchanges: 1, wantUID: "ks-uid", wantSentID: "c-9",
		},
		{name: "no token and no PAT resolves nothing"},
		{
			name: "mirror-fed env loses to the newer live mirror", mirrorFed: true, env: "dakr-A", stored: "dakr-B", pat: "dzu-pat",
			want: "dakr-B",
		},
		{
			name: "mirror-fed env is kept when the live mirror is empty", mirrorFed: true, env: "dakr-A", pat: "dzu-pat",
			want: "dakr-A",
		},
		{
			name: "mirror-fed start with nothing stored exchanges", mirrorFed: true, pat: "dzu-pat",
			want: "dakr-exchanged", wantExchanges: 1, wantUID: "ks-uid",
		},
		{
			name: "an explicit env token wins over the live mirror", env: "dakr-A", stored: "dakr-B", pat: "dzu-pat",
			want: "dakr-A",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var objs []runtime.Object
			if !tc.noKubeSystem {
				objs = append(objs, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: types.UID("ks-uid")}})
			}
			k := fake.NewSimpleClientset(objs...)
			var calls []transport.ClusterTokenExchangeRequest
			r := &clusterTokenResolver{
				k8s:        k,
				mirrorFed:  tc.mirrorFed,
				readStored: func(context.Context) string { return tc.stored },
				exchange: func(_ context.Context, req transport.ClusterTokenExchangeRequest) (transport.ClusterTokenExchangeResult, error) {
					calls = append(calls, req)
					if tc.exchangeErr != nil {
						return transport.ClusterTokenExchangeResult{}, tc.exchangeErr
					}
					return transport.ClusterTokenExchangeResult{Token: "dakr-exchanged", ClusterID: "c-1"}, nil
				},
				log: logr.Discard(),
			}

			got, err := r.resolve(context.Background(), tc.env, tc.pat, "name", "aws", tc.clusterID)
			switch {
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("want an error naming %q, got %v", tc.wantErr, err)
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected error %v", err)
			}
			if got != tc.want || len(calls) != tc.wantExchanges {
				t.Fatalf("got %q, %d exchanges; want %q, %d", got, len(calls), tc.want, tc.wantExchanges)
			}
			if tc.wantExchanges > 0 {
				req := calls[0]
				if req.KubeSystemUID != tc.wantUID || req.ClusterID != tc.wantSentID || req.PATToken != tc.pat {
					t.Fatalf("exchange request uid=%q clusterID=%q, want uid=%q clusterID=%q",
						req.KubeSystemUID, req.ClusterID, tc.wantUID, tc.wantSentID)
				}
			}
			// resolve only reads; the caller persists the token to the mirror.
			if s, _ := k.CoreV1().Secrets("").List(context.Background(), metav1.ListOptions{}); len(s.Items) != 0 {
				t.Fatalf("resolve wrote %d Secrets", len(s.Items))
			}
		})
	}
}

// Only the default Secret-mode chart, which feeds CLUSTER_TOKEN from the
// mirror Secret, marks the env token as mirror-fed. Anything else, including
// an unset marker and ConfigMap mode, keeps the env token authoritative.
func TestEnvTokenIsMirrorFed(t *testing.T) {
	cases := []struct {
		useSecret bool
		source    string
		want      bool
	}{
		{useSecret: true, source: "mirror", want: true},
		{useSecret: true, source: "", want: false},
		{useSecret: true, source: "existingSecret", want: false},
		{useSecret: true, source: "Mirror", want: false},
		{useSecret: false, source: "mirror", want: false},
		{useSecret: false, source: "", want: false},
	}
	for _, tc := range cases {
		if got := envTokenIsMirrorFed(tc.useSecret, tc.source); got != tc.want {
			t.Errorf("envTokenIsMirrorFed(%v, %q) = %v, want %v", tc.useSecret, tc.source, got, tc.want)
		}
	}
}
