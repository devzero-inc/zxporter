package networkmonitor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"

	gen "github.com/devzero-inc/zxporter/gen/api/v1"
)

// Bytes measured leaving a pod do not carry their own price. The same byte is
// free, or charged at a cross-zone rate, or at a NAT-gateway processing rate, or
// at a public-IP data-transfer rate, depending on where the node sits and
// whether it has a public address. None of that is visible from a flow record;
// it is visible only to the node, from its own instance metadata service. This
// file reads it once per process and reports it alongside the flows.
//
// Everything here is best-effort and must fail quietly. A node with no metadata
// service (bare metal, an unrecognised provider, a blocked link-local route)
// reports nothing, and nothing is exactly what the control plane must see --
// never a guessed region or an assumed NAT path.

const (
	// awsIMDSBase is the IPv4 link-local metadata endpoint, identical across AWS
	// and Azure; the two are told apart by the paths and headers they answer.
	awsIMDSBase   = "http://169.254.169.254"
	azureIMDSBase = "http://169.254.169.254"
	gcpIMDSBase   = "http://metadata.google.internal"

	// metadataTimeout bounds a single metadata request. The services are
	// link-local and answer in single-digit milliseconds when present; when
	// absent the route is usually rejected immediately, and this bound covers
	// the case where it black-holes instead.
	metadataTimeout = 2 * time.Second

	// imdsTokenTTLSeconds is the lifetime requested for an IMDSv2 token. We use
	// the token within the same call, so the shortest practical TTL is enough.
	imdsTokenTTLSeconds = "60"

	// probeRetryInterval is how long to wait before re-probing after a failed
	// attempt. A node can easily out-run its own metadata service at startup --
	// IMDS not yet serving, metadata DNS not yet resolving, the hop-limited
	// route not yet installed, or plain throttling -- and caching that first
	// failure would blind the node until the pod restarts. Successes are cached
	// forever; only failures come back here.
	probeRetryInterval = 5 * time.Minute
)

// newMetadataClient builds the HTTP client used for every metadata read.
//
// Proxy is explicitly nil. Go's default transport uses ProxyFromEnvironment, and
// a cluster that sets HTTP_PROXY/HTTPS_PROXY for its workloads would otherwise
// send these link-local requests through the proxy unless NO_PROXY happens to
// cover 169.254.169.254 and metadata.google.internal. That breaks provider
// detection, and it hands an IMDSv2 token request to a third party.
func newMetadataClient() *http.Client {
	return &http.Client{
		Timeout: metadataTimeout,
		Transport: &http.Transport{
			Proxy: nil,
		},
	}
}

// EgressPathResolver reads the node's instance metadata once and caches the
// answer for the lifetime of the process. The facts it reports -- provider,
// region, zone, VPC, subnet, whether the primary interface has a public address
// -- do not change while a node is running; a node that is re-addressed is a new
// node with a new pod.
type EgressPathResolver struct {
	log    logr.Logger
	client *http.Client
	now    func() time.Time

	// baseURLs lets tests point the resolver at an httptest server. Empty
	// entries fall back to the real link-local endpoints.
	awsBase   string
	azureBase string
	gcpBase   string

	mu sync.Mutex
	// cached holds the best answer so far, which may be incomplete. A probe can
	// succeed at reading the zone while still failing to establish the egress
	// path -- a throttled or 5xx read of public-ipv4s, an interface whose MAC
	// could not be read -- and that partial answer is worth publishing, because
	// zone, VPC and subnet are useful on their own.
	//
	// complete is what gates re-probing, NOT cached != nil. Caching a partial
	// answer permanently would defeat the retry entirely for the one case it
	// exists to cover: a node throttled at startup would report an unknown
	// egress path until its pod restarts.
	cached    *gen.NodeEgressPath
	complete  bool
	nextRetry time.Time
	probing   bool
}

// isComplete reports whether a probe result settled the one fact that cannot be
// guessed. Everything else in the message is either present or genuinely absent
// for this node; has_public_ip is the only field with a third state.
func isComplete(path *gen.NodeEgressPath) bool {
	return path != nil && path.HasPublicIp != nil //nolint:protogetter // presence is the question
}

// NewEgressPathResolver builds a resolver against the real metadata endpoints.
func NewEgressPathResolver(log logr.Logger) *EgressPathResolver {
	return &EgressPathResolver{
		log:    log,
		client: newMetadataClient(),
		now:    time.Now,
	}
}

// Cached returns the best answer so far, or nil if no probe has succeeded yet.
// The answer may be incomplete (no egress path); the control plane records that
// as unknown. It never performs IO and never blocks on the network, so it is
// safe to call from the flush path while a collection lock is held.
func (r *EgressPathResolver) Cached() *gen.NodeEgressPath {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cached
}

// Prime probes the metadata service unless a COMPLETE answer is already held,
// one is already running, or a recent attempt is still inside its retry
// interval. It performs network IO and so must never be called while holding a
// collection lock -- a black-holed link-local route costs several
// metadataTimeout waits.
//
// Safe to call repeatedly; the monitor calls it once at startup and again from
// the flush loop for as long as the answer is still incomplete.
func (r *EgressPathResolver) Prime(ctx context.Context) {
	r.mu.Lock()
	if r.complete || r.probing || r.now().Before(r.nextRetry) {
		r.mu.Unlock()
		return
	}
	r.probing = true
	r.mu.Unlock()

	path := r.probe(ctx)

	r.mu.Lock()
	r.probing = false
	switch {
	case path == nil:
		// Nothing at all. Keep whatever we had (possibly nothing) and retry.
		r.nextRetry = r.now().Add(probeRetryInterval)
		r.mu.Unlock()
		r.log.V(1).Info("no cloud instance metadata service answered; "+
			"node egress path stays unknown until the next attempt",
			"retry_in", probeRetryInterval.String())
		return

	case !isComplete(path):
		// Partial: publish it -- zone, VPC and subnet are worth reporting -- but
		// keep retrying, because the egress path is still unknown and that is
		// the field the price depends on.
		r.cached = path
		r.nextRetry = r.now().Add(probeRetryInterval)
		r.mu.Unlock()
		r.log.V(1).Info("node metadata read, but the egress path is still unknown; "+
			"will re-probe",
			"cloud_provider", path.GetCloudProvider(),
			"zone", path.GetZone(),
			"retry_in", probeRetryInterval.String())
		return

	default:
		r.cached = path
		r.complete = true
		r.mu.Unlock()
	}

	r.log.Info("resolved node egress path",
		"cloud_provider", path.GetCloudProvider(),
		"region", path.GetRegion(),
		"zone", path.GetZone(),
		"vpc_id", path.GetVpcId(),
		"subnet_id", path.GetSubnetId(),
		"has_public_ip", path.GetHasPublicIp(),
	)
}

// Resolve primes if the answer is not yet complete and returns whatever is
// known. It can block on the network; prefer Cached plus a background Prime on
// hot paths.
func (r *EgressPathResolver) Resolve(ctx context.Context) *gen.NodeEgressPath {
	r.Prime(ctx)
	return r.Cached()
}

// probe tries each provider in turn and returns the first that answers. Order is
// by fleet share; a provider that is not present rejects or times out quickly,
// and the whole probe is bounded by metadataTimeout per request.
func (r *EgressPathResolver) probe(ctx context.Context) *gen.NodeEgressPath {
	if p, err := r.resolveAWS(ctx); err == nil && p != nil {
		return p
	} else if err != nil {
		r.log.V(1).Info("AWS instance metadata unavailable", "error", err.Error())
	}

	if p, err := r.resolveGCP(ctx); err == nil && p != nil {
		return p
	} else if err != nil {
		r.log.V(1).Info("GCP instance metadata unavailable", "error", err.Error())
	}

	if p, err := r.resolveAzure(ctx); err == nil && p != nil {
		return p
	} else if err != nil {
		r.log.V(1).Info("Azure instance metadata unavailable", "error", err.Error())
	}

	return nil
}

func (r *EgressPathResolver) base(override, fallback string) string {
	if override != "" {
		return override
	}
	return fallback
}

// get performs one metadata GET and returns the trimmed body. A non-2xx status
// is an error: metadata services answer 404 for attributes an instance does not
// have (no public IP, for example), and the callers below treat that as "the
// attribute is absent", not as "the provider is absent".
func (r *EgressPathResolver) get(ctx context.Context, url string, headers map[string]string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("metadata %s: status %d", url, resp.StatusCode)
	}
	return strings.TrimSpace(string(body)), nil
}

// getOptional is get, with any failure reported as an empty string. Use it only
// where an unreadable attribute and an absent attribute deserve the same
// treatment -- never for has_public_ip, which needs the two told apart.
func (r *EgressPathResolver) getOptional(ctx context.Context, url string, headers map[string]string) string {
	v, err := r.get(ctx, url, headers)
	if err != nil {
		return ""
	}
	return v
}

// getDefinite distinguishes the three outcomes that has_public_ip depends on:
// the attribute exists (value, true), the service says it provably does not
// exist -- HTTP 404 -- ("", true), or we could not find out ("", false).
//
// The distinction is the whole point. Collapsing a timeout, a 5xx, throttling
// or an expired token into "no public address" bills that node's egress at the
// NAT rate, roughly 4x, on the strength of a failed request.
func (r *EgressPathResolver) getDefinite(ctx context.Context, url string, headers map[string]string) (string, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", false
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return "", false
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return "", false
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		// A definite "this instance has no such attribute".
		return "", true
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return strings.TrimSpace(string(body)), true
	default:
		return "", false
	}
}

// --- AWS (IMDSv2) ---

// resolveAWS walks the IMDSv2 token flow. IMDSv1 is disabled on hardened
// accounts (and on every node launched with HttpTokens=required), so the token
// is obtained first and presented on each read.
func (r *EgressPathResolver) resolveAWS(ctx context.Context) (*gen.NodeEgressPath, error) {
	base := r.base(r.awsBase, awsIMDSBase)

	tokenReq, err := http.NewRequestWithContext(ctx, http.MethodPut, base+"/latest/api/token", nil)
	if err != nil {
		return nil, err
	}
	tokenReq.Header.Set("X-aws-ec2-metadata-token-ttl-seconds", imdsTokenTTLSeconds)
	tokenResp, err := r.client.Do(tokenReq)
	if err != nil {
		return nil, fmt.Errorf("imds token: %w", err)
	}
	tokenBody, err := io.ReadAll(io.LimitReader(tokenResp.Body, 8*1024))
	_ = tokenResp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("imds token body: %w", err)
	}
	if tokenResp.StatusCode < 200 || tokenResp.StatusCode >= 300 {
		return nil, fmt.Errorf("imds token: status %d", tokenResp.StatusCode)
	}
	headers := map[string]string{"X-aws-ec2-metadata-token": strings.TrimSpace(string(tokenBody))}

	// The AZ is the one attribute we insist on. Without it there is nothing to
	// distinguish an EC2 instance from any other host that happens to answer on
	// the link-local address.
	zone, err := r.get(ctx, base+"/latest/meta-data/placement/availability-zone", headers)
	if err != nil {
		return nil, err
	}

	path := &gen.NodeEgressPath{
		CloudProvider: "aws",
		Zone:          zone,
		Region:        awsRegionFromAZ(zone),
		PrivateIp:     r.getOptional(ctx, base+"/latest/meta-data/local-ipv4", headers),
	}

	// The network attributes hang off the primary interface's MAC. A node whose
	// MAC cannot be read still reports provider, region and zone -- those are
	// worth having on their own -- but it reports no opinion at all about its
	// egress path, because it has not read the interface that would decide it.
	mac := firstMAC(r.getOptional(ctx, base+"/latest/meta-data/mac", headers))
	if mac != "" {
		ifaceBase := base + "/latest/meta-data/network/interfaces/macs/" + mac
		path.SubnetId = r.getOptional(ctx, ifaceBase+"/subnet-id", headers)
		path.VpcId = r.getOptional(ctx, ifaceBase+"/vpc-id", headers)
		// public-ipv4s is 404 on an instance with no public address, which is
		// precisely the "leaves through a NAT gateway" case -- and is a
		// different thing from a read we could not complete.
		if publicIPs, definite := r.getDefinite(ctx, ifaceBase+"/public-ipv4s", headers); definite {
			path.HasPublicIp = ptrBool(publicIPs != "")
		}
	}

	return path, nil
}

func ptrBool(v bool) *bool { return &v }

// awsRegionFromAZ derives the region from an AWS availability zone, which is the
// only place IMDS exposes it without a second call.
//
// An AWS region name always ends at its first purely numeric segment
// ("us-west-2", "eu-central-1", "us-gov-west-1"), and everything after that
// belongs to a Local or Wavelength Zone ("us-west-2-lax-1a" sits in us-west-2).
// Cutting at that segment therefore gives the parent region in every form,
// which is what data transfer is priced by. Simply stripping the trailing letter
// would return "us-west-2-lax-1" -- a region that does not exist.
//
// Anything that does not match is returned unchanged rather than truncated into
// a region that was never reported.
func awsRegionFromAZ(az string) string {
	if az == "" {
		return ""
	}
	// Drop the zone letter first; it is glued to the last segment ("2a", "1a").
	last := az[len(az)-1]
	if last < 'a' || last > 'z' {
		return az
	}
	trimmed := az[:len(az)-1]

	segments := strings.Split(trimmed, "-")
	for i, seg := range segments {
		if !isAllDigits(seg) {
			continue
		}
		return strings.Join(segments[:i+1], "-")
	}
	return az
}

func isAllDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return s != ""
}

// firstMAC takes the first entry of IMDS's newline-separated MAC list and drops
// the trailing slash IMDS puts on directory-style entries.
func firstMAC(raw string) string {
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(line), "/"))
		if line != "" {
			return line
		}
	}
	return ""
}

// --- GCP ---

type gcpNetworkInterface struct {
	IP            string `json:"ip"`
	Network       string `json:"network"`
	Subnetmask    string `json:"subnetmask"`
	Subnetwork    string `json:"subnetwork"`
	AccessConfigs []struct {
		ExternalIP string `json:"externalIp"`
		Type       string `json:"type"`
	} `json:"accessConfigs"`
}

// resolveGCP reads the recursive instance document, which returns zone, network
// interfaces and access configs in one request.
func (r *EgressPathResolver) resolveGCP(ctx context.Context) (*gen.NodeEgressPath, error) {
	base := r.base(r.gcpBase, gcpIMDSBase)
	headers := map[string]string{"Metadata-Flavor": "Google"}

	body, err := r.get(ctx, base+"/computeMetadata/v1/instance/?recursive=true", headers)
	if err != nil {
		return nil, err
	}

	var doc struct {
		Zone              string                `json:"zone"`
		NetworkInterfaces []gcpNetworkInterface `json:"networkInterfaces"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		return nil, fmt.Errorf("gcp metadata: %w", err)
	}

	zone := gcpLastPathSegment(doc.Zone)
	if zone == "" {
		return nil, fmt.Errorf("gcp metadata: no zone")
	}

	path := &gen.NodeEgressPath{
		CloudProvider: "gcp",
		Zone:          zone,
		Region:        gcpRegionFromZone(zone),
	}

	if len(doc.NetworkInterfaces) > 0 {
		nic := doc.NetworkInterfaces[0]
		path.PrivateIp = nic.IP
		// GCP has no VPC id; the network and subnetwork names are the closest
		// equivalents and are what its billing is scoped by.
		path.VpcId = gcpLastPathSegment(nic.Network)
		path.SubnetId = gcpLastPathSegment(nic.Subnetwork)
		// The recursive instance document either parsed or it did not, and it
		// did, so the access-config list is authoritative: an external IP is
		// present or it provably is not.
		hasPublic := false
		for _, ac := range nic.AccessConfigs {
			if ac.ExternalIP != "" {
				hasPublic = true
				break
			}
		}
		path.HasPublicIp = ptrBool(hasPublic)
	}

	return path, nil
}

// gcpLastPathSegment reduces GCP's fully-qualified resource paths
// ("projects/123/zones/us-central1-a") to the bare name.
func gcpLastPathSegment(v string) string {
	if v == "" {
		return ""
	}
	if i := strings.LastIndex(v, "/"); i >= 0 {
		return v[i+1:]
	}
	return v
}

// gcpRegionFromZone drops the trailing zone suffix: "us-central1-a" ->
// "us-central1".
func gcpRegionFromZone(zone string) string {
	if i := strings.LastIndex(zone, "-"); i > 0 {
		return zone[:i]
	}
	return zone
}

// --- Azure ---

// resolveAzure reads the instance document. Azure reports the zone as a bare
// ordinal ("1") scoped to the region, so it is qualified here as "<region>-<n>"
// to stay unambiguous alongside the other providers.
func (r *EgressPathResolver) resolveAzure(ctx context.Context) (*gen.NodeEgressPath, error) {
	base := r.base(r.azureBase, azureIMDSBase)
	headers := map[string]string{"Metadata": "true"}

	body, err := r.get(ctx, base+"/metadata/instance?api-version=2021-02-01", headers)
	if err != nil {
		return nil, err
	}

	var doc struct {
		Compute struct {
			Location string `json:"location"`
			Zone     string `json:"zone"`
		} `json:"compute"`
		Network struct {
			Interface []struct {
				IPv4 struct {
					IPAddress []struct {
						PrivateIPAddress string `json:"privateIpAddress"`
						PublicIPAddress  string `json:"publicIpAddress"`
					} `json:"ipAddress"`
					Subnet []struct {
						Address string `json:"address"`
						Prefix  string `json:"prefix"`
					} `json:"subnet"`
				} `json:"ipv4"`
			} `json:"interface"`
		} `json:"network"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		return nil, fmt.Errorf("azure metadata: %w", err)
	}
	if doc.Compute.Location == "" {
		return nil, fmt.Errorf("azure metadata: no location")
	}

	path := &gen.NodeEgressPath{
		CloudProvider: "azure",
		Region:        doc.Compute.Location,
	}
	if doc.Compute.Zone != "" {
		path.Zone = doc.Compute.Location + "-" + doc.Compute.Zone
	}

	if len(doc.Network.Interface) > 0 {
		// An interface can carry several IPv4 configurations, and the public one
		// is not necessarily the first: indexing [0] reports a node as private
		// when it is not, which bills its egress at the NAT rate. Scan every
		// address on every interface -- a public address anywhere means this
		// node can leave the VPC directly.
		hasPublic := false
		for _, iface := range doc.Network.Interface {
			for _, addr := range iface.IPv4.IPAddress {
				if addr.PublicIPAddress != "" {
					hasPublic = true
					break
				}
			}
			if hasPublic {
				break
			}
		}
		// The document parsed, so this is a definite answer either way -- unlike
		// AWS, there is no per-attribute read that could have failed on its own.
		path.HasPublicIp = ptrBool(hasPublic)

		// Placement comes from the primary interface.
		primary := doc.Network.Interface[0]
		for _, addr := range primary.IPv4.IPAddress {
			if addr.PrivateIPAddress != "" {
				path.PrivateIp = addr.PrivateIPAddress
				break
			}
		}
		// Azure IMDS reports the subnet as an address/prefix pair rather than a
		// resource id; the CIDR is what identifies it here.
		if len(primary.IPv4.Subnet) > 0 && primary.IPv4.Subnet[0].Address != "" {
			path.SubnetId = primary.IPv4.Subnet[0].Address + "/" + primary.IPv4.Subnet[0].Prefix
		}
	}

	return path, nil
}
