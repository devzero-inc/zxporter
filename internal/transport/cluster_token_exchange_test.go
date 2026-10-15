package transport

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"
	"github.com/go-logr/logr"

	gen "github.com/devzero-inc/zxporter/gen/api/v1"
	"github.com/devzero-inc/zxporter/gen/api/v1/apiv1connect"
)

type exchangeServer struct {
	apiv1connect.UnimplementedClusterServiceHandler
	got  *gen.CreateClusterTokenRequest
	auth string
}

func (s *exchangeServer) CreateClusterToken(_ context.Context, req *connect.Request[gen.CreateClusterTokenRequest]) (*connect.Response[gen.CreateClusterTokenResponse], error) {
	s.got, s.auth = req.Msg, req.Header().Get("Authorization")
	return connect.NewResponse(&gen.CreateClusterTokenResponse{Token: "dakr-new", ClusterId: "c-1", Reattached: true}), nil
}

func TestExchangePATForClusterTokenSendsFingerprintAndID(t *testing.T) {
	srv := &exchangeServer{}
	mux := http.NewServeMux()
	mux.Handle(apiv1connect.NewClusterServiceHandler(srv))
	ts := httptest.NewUnstartedServer(mux)
	ts.EnableHTTP2 = true
	ts.StartTLS()
	defer ts.Close()

	c := NewDakrClient(ts.URL, "", logr.Discard()).(*RealDakrClient)
	c.clusterClient = apiv1connect.NewClusterServiceClient(ts.Client(), ts.URL)

	res, err := c.ExchangePATForClusterToken(context.Background(), ClusterTokenExchangeRequest{
		PATToken: "dzu-x", ClusterName: "n", K8sProvider: "aws", KubeSystemUID: "uid-1", ClusterID: "c-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if srv.auth != "Bearer dzu-x" || srv.got.KubeSystemUid != "uid-1" || srv.got.ClusterId != "c-1" {
		t.Fatalf("sent auth=%q uid=%q id=%q", srv.auth, srv.got.KubeSystemUid, srv.got.ClusterId)
	}
	if res != (ClusterTokenExchangeResult{Token: "dakr-new", ClusterID: "c-1", Reattached: true}) {
		t.Fatalf("result %+v", res)
	}
}
