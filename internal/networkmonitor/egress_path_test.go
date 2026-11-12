package networkmonitor

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-logr/logr"
)

// testAZ is the availability zone the AWS fixtures report; its region half is
// what awsRegionFromAZ is expected to derive.
const (
	testAZ     = "us-west-2a"
	testRegion = "us-west-2"
)

// newTestResolver points a resolver at an httptest server for one provider and
// at a dead address for the others, so a probe cannot reach the real link-local
// metadata service from a developer machine or from CI.
func newTestResolver(provider, baseURL string) *EgressPathResolver {
	r := &EgressPathResolver{
		log:    logr.Discard(),
		client: newMetadataClient(),
		now:    time.Now,
		// An address that refuses immediately, so an unmatched provider fails
		// fast instead of hanging on the real 169.254.169.254.
		awsBase:   "http://127.0.0.1:1",
		gcpBase:   "http://127.0.0.1:1",
		azureBase: "http://127.0.0.1:1",
	}
	switch provider {
	case "aws":
		r.awsBase = baseURL
	case "gcp":
		r.gcpBase = baseURL
	case "azure":
		r.azureBase = baseURL
	}
	return r
}

// awsIMDSFixture is an EC2 instance as IMDSv2 answers for it: the token flow,
// the AZ, the MAC list, and the per-interface network attributes. publicIPs
// empty means the instance has no public address, in which case IMDS returns
// 404 for that path -- which is how a private-subnet node is recognised.
func awsIMDSFixture(t *testing.T, az, mac, vpc, subnet, privateIP, publicIPs string) *httptest.Server {
	t.Helper()
	const token = "AQAEALb-fixture-token=="
	mux := http.NewServeMux()
	mux.HandleFunc("/latest/api/token", func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPut {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if req.Header.Get("X-aws-ec2-metadata-token-ttl-seconds") == "" {
			// IMDSv2 rejects a token request without a TTL; asserting it here
			// keeps the client honest about the handshake it claims to speak.
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(token))
	})
	guarded := func(body string) http.HandlerFunc {
		return func(w http.ResponseWriter, req *http.Request) {
			if req.Header.Get("X-aws-ec2-metadata-token") != token {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if body == "" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(body))
		}
	}
	mux.HandleFunc("/latest/meta-data/placement/availability-zone", guarded(az))
	mux.HandleFunc("/latest/meta-data/mac", guarded(mac+"/\n"))
	mux.HandleFunc("/latest/meta-data/local-ipv4", guarded(privateIP))
	base := "/latest/meta-data/network/interfaces/macs/" + mac
	mux.HandleFunc(base+"/subnet-id", guarded(subnet))
	mux.HandleFunc(base+"/vpc-id", guarded(vpc))
	mux.HandleFunc(base+"/public-ipv4s", guarded(publicIPs))

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestResolveAWS(t *testing.T) {
	t.Run("a private-subnet node reports the NAT-bound shape", func(t *testing.T) {
		srv := awsIMDSFixture(t, testAZ, "0a:1b:2c:3d:4e:5f", "vpc-0abc123", "subnet-0def456", "10.0.1.20", "")
		r := newTestResolver("aws", srv.URL)

		got := r.Resolve(t.Context())
		if got == nil {
			t.Fatal("expected an egress path")
		}
		if got.GetCloudProvider() != "aws" {
			t.Errorf("cloud_provider = %q", got.GetCloudProvider())
		}
		if got.GetZone() != testAZ {
			t.Errorf("zone = %q", got.GetZone())
		}
		if got.GetRegion() != testRegion {
			t.Errorf("region = %q, want it derived from the AZ", got.GetRegion())
		}
		if got.GetVpcId() != "vpc-0abc123" {
			t.Errorf("vpc_id = %q", got.GetVpcId())
		}
		if got.GetSubnetId() != "subnet-0def456" {
			t.Errorf("subnet_id = %q", got.GetSubnetId())
		}
		if got.GetPrivateIp() != "10.0.1.20" {
			t.Errorf("private_ip = %q", got.GetPrivateIp())
		}
		// The fact the whole change exists for: no public address means egress
		// is processed by a NAT gateway, at roughly 4x the direct rate. A 404 is
		// a definite answer, so presence must be set, not merely falsy.
		if got.HasPublicIp == nil {
			t.Fatal("a 404 on public-ipv4s is a definite 'no public address'")
		}
		if got.GetHasPublicIp() {
			t.Error("has_public_ip should be false when IMDS 404s public-ipv4s")
		}
	})

	t.Run("a node with a public address reports it", func(t *testing.T) {
		srv := awsIMDSFixture(t, "eu-central-1b", "aa:bb:cc:dd:ee:ff", "vpc-pub", "subnet-pub", "10.0.2.30", "52.1.2.3")
		r := newTestResolver("aws", srv.URL)

		got := r.Resolve(t.Context())
		if got == nil {
			t.Fatal("expected an egress path")
		}
		if got.HasPublicIp == nil || !got.GetHasPublicIp() {
			t.Error("has_public_ip should be a definite true when public-ipv4s is non-empty")
		}
		if got.GetRegion() != "eu-central-1" {
			t.Errorf("region = %q", got.GetRegion())
		}
	})

	t.Run("no metadata service means no claim at all", func(t *testing.T) {
		r := newTestResolver("none", "")
		if got := r.Resolve(t.Context()); got != nil {
			t.Errorf("expected nil, got %+v", got)
		}
	})

	t.Run("the answer is cached for the process lifetime", func(t *testing.T) {
		var hits int
		mux := http.NewServeMux()
		mux.HandleFunc("/latest/api/token", func(w http.ResponseWriter, _ *http.Request) {
			hits++
			_, _ = w.Write([]byte("tok"))
		})
		mux.HandleFunc("/latest/meta-data/placement/availability-zone", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("us-east-1a"))
		})
		mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
		srv := httptest.NewServer(mux)
		defer srv.Close()

		r := newTestResolver("aws", srv.URL)
		first := r.Resolve(t.Context())
		second := r.Resolve(t.Context())

		if first == nil || second == nil {
			t.Fatal("expected an egress path from both calls")
		}
		if hits != 1 {
			t.Errorf("metadata probed %d times, want 1 -- the node's placement does not change while it runs", hits)
		}
	})
}

// TestResolveAWSUnknownPublicIP covers the outcomes that are NOT a definite
// answer. Each one must leave has_public_ip absent, because the alternative --
// reporting false -- makes the control plane bill the node's egress at the NAT
// rate on the strength of a failed request.
func TestResolveAWSUnknownPublicIP(t *testing.T) {
	const (
		token = "tok"
		mac   = "0a:1b:2c:3d:4e:5f"
	)

	// publicIPStatus is what the metadata service does for public-ipv4s;
	// serveMAC controls whether the interface can be found at all.
	newSrv := func(t *testing.T, publicIPStatus int, serveMAC bool) *EgressPathResolver {
		t.Helper()
		mux := http.NewServeMux()
		mux.HandleFunc("/latest/api/token", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(token))
		})
		mux.HandleFunc("/latest/meta-data/placement/availability-zone", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(testAZ))
		})
		mux.HandleFunc("/latest/meta-data/mac", func(w http.ResponseWriter, _ *http.Request) {
			if !serveMAC {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, _ = w.Write([]byte(mac + "/\n"))
		})
		ifaceBase := "/latest/meta-data/network/interfaces/macs/" + mac
		mux.HandleFunc(ifaceBase+"/subnet-id", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("subnet-0def456"))
		})
		mux.HandleFunc(ifaceBase+"/vpc-id", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("vpc-0abc123"))
		})
		mux.HandleFunc(ifaceBase+"/public-ipv4s", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(publicIPStatus)
		})
		mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)
		return newTestResolver("aws", srv.URL)
	}

	cases := []struct {
		name             string
		publicIPStatus   int
		serveMAC         bool
		wantSubnetFilled bool
	}{
		{"a throttled read is not a missing address", http.StatusTooManyRequests, true, true},
		{"a server error is not a missing address", http.StatusInternalServerError, true, true},
		{"an expired token is not a missing address", http.StatusUnauthorized, true, true},
		// Without the MAC there is no interface to ask, so nothing is known
		// about the egress path -- and nothing about the subnet either.
		{"an unreadable interface is not a missing address", http.StatusOK, false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := newSrv(t, tc.publicIPStatus, tc.serveMAC).Resolve(t.Context())
			if got == nil {
				t.Fatal("the AZ resolved, so the node still reports a path")
			}
			if got.HasPublicIp != nil {
				t.Errorf("has_public_ip = %v, want absent (unknown)", got.GetHasPublicIp())
			}
			// Region and zone are still worth having on their own.
			if got.GetZone() != testAZ {
				t.Errorf("zone = %q", got.GetZone())
			}
			if gotFilled := got.GetSubnetId() != ""; gotFilled != tc.wantSubnetFilled {
				t.Errorf("subnet filled = %v, want %v", gotFilled, tc.wantSubnetFilled)
			}
		})
	}
}

// TestProbeFailureIsRetried pins the lifecycle rule: a success is cached
// forever, a failure is not. A node that starts before its metadata service is
// serving would otherwise report an unknown egress path until the pod restarts.
func TestProbeFailureIsRetried(t *testing.T) {
	var attempts int
	var serving bool
	mux := http.NewServeMux()
	mux.HandleFunc("/latest/api/token", func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		if !serving {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("tok"))
	})
	mux.HandleFunc("/latest/meta-data/placement/availability-zone", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("us-east-1a"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	clock := time.Now()
	r := newTestResolver("aws", srv.URL)
	r.now = func() time.Time { return clock }

	// IMDS is not up yet.
	if got := r.Resolve(t.Context()); got != nil {
		t.Fatal("expected no egress path while the metadata service is down")
	}
	// Inside the retry interval nothing is re-probed, so a flushing node does
	// not hammer the metadata service every few seconds.
	before := attempts
	r.Resolve(t.Context())
	if attempts != before {
		t.Errorf("re-probed inside the retry interval (%d -> %d)", before, attempts)
	}

	// The metadata service comes up and the retry interval elapses.
	serving = true
	clock = clock.Add(probeRetryInterval + time.Second)
	got := r.Resolve(t.Context())
	if got == nil {
		t.Fatal("expected the retry to succeed once the metadata service is up")
	}
	if got.GetZone() != "us-east-1a" {
		t.Errorf("zone = %q", got.GetZone())
	}

	// And the success sticks.
	after := attempts
	r.Resolve(t.Context())
	if attempts != after {
		t.Errorf("re-probed after a success (%d -> %d)", after, attempts)
	}
}

// TestMetadataClientIgnoresProxy pins that link-local metadata reads never go
// through HTTP_PROXY/HTTPS_PROXY. Go's default transport uses
// ProxyFromEnvironment, so on a proxied cluster an IMDSv2 token request would
// otherwise be handed to a third party and provider detection would break.
func TestMetadataClientIgnoresProxy(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://proxy.invalid:3128")
	t.Setenv("HTTPS_PROXY", "http://proxy.invalid:3128")

	transport, ok := newMetadataClient().Transport.(*http.Transport)
	if !ok {
		t.Fatal("expected an *http.Transport")
	}
	if transport.Proxy != nil {
		t.Error("metadata client must not consult the environment proxy")
	}

	// And it actually reaches a server while those are set.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(testAZ))
	}))
	defer srv.Close()

	r := newTestResolver("aws", srv.URL)
	if got := r.Resolve(t.Context()); got == nil || got.GetZone() != testAZ {
		t.Error("metadata read did not reach the server with a proxy configured")
	}
}

func TestResolveGCP(t *testing.T) {
	const doc = `{
	  "zone": "projects/123456789/zones/us-central1-a",
	  "networkInterfaces": [
	    {
	      "ip": "10.128.0.7",
	      "network": "projects/123456789/networks/prod-vpc",
	      "subnetwork": "projects/123456789/regions/us-central1/subnetworks/prod-nodes",
	      "accessConfigs": []
	    }
	  ]
	}`

	newGCP := func(t *testing.T, body string) *EgressPathResolver {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.Header.Get("Metadata-Flavor") != "Google" {
				// GCP's metadata server requires this header; without it the
				// request must not be mistaken for a successful probe.
				w.WriteHeader(http.StatusForbidden)
				return
			}
			_, _ = w.Write([]byte(body))
		}))
		t.Cleanup(srv.Close)
		return newTestResolver("gcp", srv.URL)
	}

	t.Run("a node with no external access config leaves through Cloud NAT", func(t *testing.T) {
		got := newGCP(t, doc).Resolve(t.Context())
		if got == nil {
			t.Fatal("expected an egress path")
		}
		if got.GetCloudProvider() != "gcp" {
			t.Errorf("cloud_provider = %q", got.GetCloudProvider())
		}
		if got.GetZone() != "us-central1-a" {
			t.Errorf("zone = %q, want the bare zone name", got.GetZone())
		}
		if got.GetRegion() != "us-central1" {
			t.Errorf("region = %q", got.GetRegion())
		}
		if got.GetVpcId() != "prod-vpc" {
			t.Errorf("vpc_id = %q, want the network name", got.GetVpcId())
		}
		if got.GetSubnetId() != "prod-nodes" {
			t.Errorf("subnet_id = %q, want the subnetwork name", got.GetSubnetId())
		}
		if got.GetPrivateIp() != "10.128.0.7" {
			t.Errorf("private_ip = %q", got.GetPrivateIp())
		}
		if got.HasPublicIp == nil {
			t.Fatal("a parsed instance document is a definite answer")
		}
		if got.GetHasPublicIp() {
			t.Error("has_public_ip should be false with no access configs")
		}
	})

	t.Run("an external access config means a public address", func(t *testing.T) {
		const withExternal = `{
		  "zone": "projects/1/zones/europe-west1-b",
		  "networkInterfaces": [
		    {
		      "ip": "10.132.0.3",
		      "network": "projects/1/networks/default",
		      "subnetwork": "projects/1/regions/europe-west1/subnetworks/default",
		      "accessConfigs": [{"externalIp": "34.1.2.3", "type": "ONE_TO_ONE_NAT"}]
		    }
		  ]
		}`
		got := newGCP(t, withExternal).Resolve(t.Context())
		if got == nil {
			t.Fatal("expected an egress path")
		}
		if got.HasPublicIp == nil || !got.GetHasPublicIp() {
			t.Error("has_public_ip should be a definite true when an access config carries an external IP")
		}
		if got.GetRegion() != "europe-west1" {
			t.Errorf("region = %q", got.GetRegion())
		}
	})
}

func TestResolveAzure(t *testing.T) {
	const doc = `{
	  "compute": {"location": "westus2", "zone": "1"},
	  "network": {
	    "interface": [
	      {
	        "ipv4": {
	          "ipAddress": [{"privateIpAddress": "10.240.0.4", "publicIpAddress": ""}],
	          "subnet": [{"address": "10.240.0.0", "prefix": "16"}]
	        }
	      }
	    ]
	  }
	}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Metadata") != "true" {
			// Azure IMDS refuses without this header, which is what stops a
			// browser from reaching it. Honour the same contract.
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(doc))
	}))
	defer srv.Close()

	got := newTestResolver("azure", srv.URL).Resolve(t.Context())
	if got == nil {
		t.Fatal("expected an egress path")
	}
	if got.GetCloudProvider() != "azure" {
		t.Errorf("cloud_provider = %q", got.GetCloudProvider())
	}
	if got.GetRegion() != "westus2" {
		t.Errorf("region = %q", got.GetRegion())
	}
	// Azure reports the zone as a bare ordinal scoped to the region, so it is
	// qualified to stay comparable with the other providers' zone names.
	if got.GetZone() != "westus2-1" {
		t.Errorf("zone = %q, want the region-qualified form", got.GetZone())
	}
	if got.GetPrivateIp() != "10.240.0.4" {
		t.Errorf("private_ip = %q", got.GetPrivateIp())
	}
	if got.GetSubnetId() != "10.240.0.0/16" {
		t.Errorf("subnet_id = %q, want the CIDR Azure reports instead of a resource id", got.GetSubnetId())
	}
	if got.HasPublicIp == nil {
		t.Fatal("a parsed instance document is a definite answer")
	}
	if got.GetHasPublicIp() {
		t.Error("has_public_ip should be false for an empty publicIpAddress")
	}
}

func TestAWSRegionFromAZ(t *testing.T) {
	cases := []struct {
		az   string
		want string
	}{
		{testAZ, testRegion},
		{"eu-central-1b", "eu-central-1"},
		{"ap-southeast-4c", "ap-southeast-4"},
		// A Local Zone belongs to a parent region; data transfer is priced by
		// that region, not by the zone group. Naively stripping the trailing
		// letter would produce "us-west-2-lax-1", which is not a region.
		{"us-west-2-lax-1a", "us-west-2"},
		// GovCloud has four segments and is still a region in its own right.
		{"us-gov-west-1a", "us-gov-west-1"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := awsRegionFromAZ(tc.az); got != tc.want {
			t.Errorf("awsRegionFromAZ(%q) = %q, want %q", tc.az, got, tc.want)
		}
	}
}

func TestFirstMAC(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"0a:1b:2c:3d:4e:5f/", "0a:1b:2c:3d:4e:5f"},
		{"0a:1b:2c:3d:4e:5f/\n0a:ff:ff:ff:ff:ff/", "0a:1b:2c:3d:4e:5f"},
		{"\n\n0a:1b:2c:3d:4e:5f", "0a:1b:2c:3d:4e:5f"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := firstMAC(tc.raw); got != tc.want {
			t.Errorf("firstMAC(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

// TestIncompleteProbeIsRetried is the case the tri-state fix and the retry fix
// each get right alone and used to get wrong together. resolveAWS returns a
// non-nil path as soon as the AZ reads, so a throttled public-ipv4s read yields
// a result that is non-nil but has no egress path. Caching that permanently --
// the obvious "cached != nil" gate -- leaves the node reporting an unknown
// egress path until its pod restarts, which is exactly what probeRetryInterval
// exists to prevent.
//
// The published answer must still carry zone, VPC and subnet in the meantime:
// those read fine and are useful on their own.
func TestIncompleteProbeIsRetried(t *testing.T) {
	const (
		token = "tok"
		mac   = "0a:1b:2c:3d:4e:5f"
	)
	var (
		publicIPCalls  int
		publicIPStatus = http.StatusTooManyRequests
	)

	mux := http.NewServeMux()
	mux.HandleFunc("/latest/api/token", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(token))
	})
	mux.HandleFunc("/latest/meta-data/placement/availability-zone", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(testAZ))
	})
	mux.HandleFunc("/latest/meta-data/mac", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(mac + "/\n"))
	})
	ifaceBase := "/latest/meta-data/network/interfaces/macs/" + mac
	mux.HandleFunc(ifaceBase+"/subnet-id", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("subnet-0def456"))
	})
	mux.HandleFunc(ifaceBase+"/vpc-id", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("vpc-0abc123"))
	})
	mux.HandleFunc(ifaceBase+"/public-ipv4s", func(w http.ResponseWriter, _ *http.Request) {
		publicIPCalls++
		if publicIPStatus != http.StatusOK {
			w.WriteHeader(publicIPStatus)
			return
		}
		_, _ = w.Write([]byte("52.1.2.3"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	clock := time.Now()
	r := newTestResolver("aws", srv.URL)
	r.now = func() time.Time { return clock }

	// First probe: throttled on public-ipv4s, so the egress path is unknown.
	first := r.Resolve(t.Context())
	if first == nil {
		t.Fatal("the AZ read fine, so a partial answer should still be published")
	}
	if first.HasPublicIp != nil {
		t.Error("a throttled read must not produce an egress path")
	}
	// The partial answer is still worth having.
	if first.GetZone() != testAZ || first.GetVpcId() != "vpc-0abc123" || first.GetSubnetId() != "subnet-0def456" {
		t.Errorf("partial answer dropped usable placement: %+v", first)
	}

	// Inside the retry interval nothing re-probes.
	before := publicIPCalls
	r.Resolve(t.Context())
	if publicIPCalls != before {
		t.Errorf("re-probed inside the retry interval (%d -> %d)", before, publicIPCalls)
	}

	// IMDS stops throttling and the interval elapses: the answer must upgrade.
	publicIPStatus = http.StatusOK
	clock = clock.Add(probeRetryInterval + time.Second)
	second := r.Resolve(t.Context())
	if second.HasPublicIp == nil {
		t.Fatal("an incomplete answer was cached permanently; the retry never fired")
	}
	if !second.GetHasPublicIp() {
		t.Error("has_public_ip should be a definite true once public-ipv4s answers")
	}

	// And now that it is definite, it sticks.
	after := publicIPCalls
	r.Resolve(t.Context())
	if publicIPCalls != after {
		t.Errorf("re-probed after a complete answer (%d -> %d)", after, publicIPCalls)
	}
}

// TestResolveAzureScansAllAddresses pins that the public address is found
// wherever it sits in the interface's address list. Indexing [0] reports a node
// as private when it is not, and a node wrongly marked private is billed NAT at
// roughly 4x.
func TestResolveAzureScansAllAddresses(t *testing.T) {
	// The public address is on the SECOND ipAddress entry.
	const doc = `{
	  "compute": {"location": "westus2", "zone": "1"},
	  "network": {
	    "interface": [
	      {
	        "ipv4": {
	          "ipAddress": [
	            {"privateIpAddress": "10.240.0.4", "publicIpAddress": ""},
	            {"privateIpAddress": "10.240.0.5", "publicIpAddress": "20.1.2.3"}
	          ],
	          "subnet": [{"address": "10.240.0.0", "prefix": "16"}]
	        }
	      }
	    ]
	  }
	}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Metadata") != "true" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(doc))
	}))
	defer srv.Close()

	got := newTestResolver("azure", srv.URL).Resolve(t.Context())
	if got == nil {
		t.Fatal("expected an egress path")
	}
	if got.HasPublicIp == nil || !got.GetHasPublicIp() {
		t.Error("a public address on a later ipAddress entry must still count")
	}
	// Placement still comes from the primary configuration.
	if got.GetPrivateIp() != "10.240.0.4" {
		t.Errorf("private_ip = %q, want the primary address", got.GetPrivateIp())
	}
	if got.GetSubnetId() != "10.240.0.0/16" {
		t.Errorf("subnet_id = %q", got.GetSubnetId())
	}
}
