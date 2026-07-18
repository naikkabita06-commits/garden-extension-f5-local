package f5

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"golang.org/x/time/rate"
)

// Backend represents a single backend (pool member) for the control-plane VIP.
type Backend struct {
	IP   string
	Port int32
}

// HTTPStatusError is returned when the server responds with a non-2xx status.
// It is used by callers to classify errors (e.g. Unauthorized vs NotFound).
type HTTPStatusError struct {
	Method          string
	URL             string
	StatusCode      int
	Status          string
	RequestID       string
	WWWAuthenticate string
	BodySnippet     string
}

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf(
		"%s %s: unexpected status %d (%s) reqID=%q wwwAuth=%q body=%s",
		e.Method,
		e.URL,
		e.StatusCode,
		e.Status,
		e.RequestID,
		e.WWWAuthenticate,
		e.BodySnippet,
	)
}

func IsNotFound(err error) bool {
	var se *HTTPStatusError
	return err != nil && AsStatusError(err, &se) && se.StatusCode == http.StatusNotFound
}

func IsUnauthorized(err error) bool {
	var se *HTTPStatusError
	return err != nil && AsStatusError(err, &se) && (se.StatusCode == http.StatusUnauthorized || se.StatusCode == http.StatusForbidden)
}

// RateLimitedError is returned when CMP responds with HTTP 429 Too Many Requests.
// The RetryAfter field contains the duration the caller should wait before retrying.
// Controller-runtime callers should use ctrl.Result{RequeueAfter: err.RetryAfter}.
type RateLimitedError struct {
	RetryAfter time.Duration
	Message    string
}

func (e *RateLimitedError) Error() string {
	return fmt.Sprintf("CMP rate limit exceeded; retry after %s: %s", e.RetryAfter, e.Message)
}

// IsRateLimited reports whether err is a CMP 429 rate limit error.
func IsRateLimited(err error) (*RateLimitedError, bool) {
	var rle *RateLimitedError
	if errors.As(err, &rle) {
		return rle, true
	}
	return nil, false
}

func AsStatusError(err error, target **HTTPStatusError) bool {
	var se *HTTPStatusError
	if errors.As(err, &se) {
		*target = se
		return true
	}
	return false
}

// Client is a thin abstraction for talking to BIG-IP.
// Story 2/3: high-level helpers plus primitive operations.
type Client interface {
	// Probe performs a non-mutating HTTP request to validate connectivity and
	// authentication to the configured endpoint.
	//
	// It returns a result for any HTTP response (including non-2xx). An error is
	// returned only for transport / request construction failures.
	Probe(ctx context.Context) (*ProbeResult, error)

	// High-level control-plane helpers
	EnsureControlPlaneVirtualServer(ctx context.Context, vip string, port int32, backends []Backend) (*CMPResourceIDs, error)
	DeleteControlPlaneVirtualServer(ctx context.Context, ids *CMPResourceIDs) error

	// CMP LBaaS (Swagger: /load-balancers/)
	ListLoadBalancers(ctx context.Context, opts *ListLoadBalancersOptions) ([]json.RawMessage, error)
	CreateLoadBalancer(ctx context.Context, form url.Values) (json.RawMessage, error)
	GetLoadBalancer(ctx context.Context, id string) (json.RawMessage, error)
	UpdateLoadBalancer(ctx context.Context, id string, form url.Values) (json.RawMessage, error)
	DeleteLoadBalancer(ctx context.Context, id string) error
	OptionsLoadBalancers(ctx context.Context) ([]string, error)

	// CMP LBaaS (UI endpoint: /load-balancers/lb_service/)
	// This is a higher-level LBService API used by some CMP UI deployments.
	ListLBServices(ctx context.Context, opts *ListLoadBalancersOptions) ([]json.RawMessage, error)
	CreateLBService(ctx context.Context, form url.Values) (json.RawMessage, error)
	GetLBService(ctx context.Context, id string) (json.RawMessage, error)
	DeleteLBService(ctx context.Context, id string) error

	// CMP LBaaS VIP (v2.1: /load-balancers/lb_service/{id}/vip)
	CreateLBServiceVIP(ctx context.Context, lbServiceID string) (json.RawMessage, error)
	GetLBServiceVIPs(ctx context.Context, lbServiceID string) ([]json.RawMessage, error)
	DeleteLBServiceVIP(ctx context.Context, lbServiceID, vipID string) error

	// CMP LBaaS Virtual Server (v2.1: /load-balancers/{lb_service_id}/virtual-servers)
	ListLBVirtualServers(ctx context.Context, lbServiceID string) ([]json.RawMessage, error)
	CreateLBVirtualServer(ctx context.Context, lbServiceID string, query url.Values) (json.RawMessage, error)
	GetLBVirtualServer(ctx context.Context, lbServiceID, vsID string) (json.RawMessage, error)
	DeleteLBVirtualServer(ctx context.Context, lbServiceID, vsID string) error

	// CMP Network API (v2.1: /networks/ports/search-by-ip/)
	SearchNetworkPortsByIP(ctx context.Context, ip string) ([]json.RawMessage, error)

	// CMP LBaaS Flavors
	ListLBFlavors(ctx context.Context) ([]json.RawMessage, error)

	// SetCMPLBaaSConfig configures CMP LBaaS provisioning parameters needed for
	// the LBService → VIP → VirtualServer flow.
	SetCMPLBaaSConfig(cfg CMPLBaaSConfig)
}

// ListLoadBalancersOptions configures GET /load-balancers/ query parameters.
//
// All fields are optional; if omitted, the server defaults apply.
type ListLoadBalancersOptions struct {
	Limit  *int32
	Offset *int32
	Field  string
	Order  string
	Search string
}

// CMPLBaaSConfig holds the CMP LBaaS provisioning parameters.
type CMPLBaaSConfig struct {
	FlavorID         int32
	NetworkID        string
	VPCID            string
	VPCName          string
	RoutingAlgorithm string
	MonitorInterval  int32
}

// CMPResourceIDs holds the IDs of CMP resources created during provisioning.
// Used to track resources for cleanup and status reporting.
type CMPResourceIDs struct {
	LBServiceID       string
	VIPPortID         string
	VirtualServerID   string
	VirtualServerName string
}

// SetCMPLBaaSConfig updates the CMP LBaaS provisioning parameters.
func (c *client) SetCMPLBaaSConfig(cfg CMPLBaaSConfig) {
	c.cpFlavorID = cfg.FlavorID
	c.cpNetworkID = cfg.NetworkID
	c.cpVPCID = cfg.VPCID
	c.cpVPCName = cfg.VPCName
	c.cpRoutingAlgorithm = cfg.RoutingAlgorithm
	if c.cpRoutingAlgorithm == "" {
		c.cpRoutingAlgorithm = "round_robin"
	}
	c.cpMonitorInterval = cfg.MonitorInterval
	if c.cpMonitorInterval <= 0 {
		c.cpMonitorInterval = 30
	}
}

// client is a concrete implementation of Client.
type client struct {
	log       logr.Logger
	endpoint  string
	partition string

	apiPrefix   string
	probePath   string
	probeMethod string

	organisationName string
	projectID        string
	ceAuth           string
	extraHeaders     http.Header

	// CMP LBaaS provisioning parameters (set via SetCMPLBaaSConfig).
	cpFlavorID         int32
	cpNetworkID        string
	cpVPCID            string
	cpVPCName          string
	cpRoutingAlgorithm string
	cpMonitorInterval  int32

	httpClient *http.Client
	username   string
	password   string

	// rateLimiter caps the outbound CMP API request rate to avoid triggering
	// HTTP 429 responses when many Shoots are reconciled simultaneously.
	// Default: 10 requests/second with a burst of 20.
	rateLimiter *rate.Limiter
}

func newHTTPClientFromEnv() *http.Client {
	// Align with common curl usage (-k) via env var.
	// IMPORTANT: Only enable this in trusted environments.
	insecure := parseEnvBool("F5_INSECURE_SKIP_TLS_VERIFY")

	transport := http.DefaultTransport.(*http.Transport).Clone()
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{}
	}
	transport.TLSClientConfig.InsecureSkipVerify = insecure //nolint:gosec // Explicitly controlled by env var.

	// If a CA bundle path is provided, load it so private/internal CA certificates are trusted
	// without having to disable TLS verification entirely.
	if caPath := strings.TrimSpace(os.Getenv("F5_CA_BUNDLE_PATH")); caPath != "" {
		if pem, err := os.ReadFile(caPath); err == nil {
			pool := x509.NewCertPool()
			pool.AppendCertsFromPEM(pem)
			transport.TLSClientConfig.RootCAs = pool
		}
	}

	return &http.Client{
		Timeout:   15 * time.Second,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// CMP/CCP endpoints can redirect to internal service hostnames
			// (e.g. neutron-service) which are not resolvable from outside.
			return http.ErrUseLastResponse
		},
	}
}

func parseEnvBool(key string) bool {
	val := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	switch val {
	case "1", "true", "yes", "y", "on":
		return true
	default:
		return false
	}
}

func parseEnvString(key, defaultVal string) string {
	val := strings.TrimSpace(os.Getenv(key))
	if val == "" {
		return defaultVal
	}
	return val
}

func normalizePrefix(prefix string) string {
	prefix = strings.TrimSpace(prefix)
	prefix = strings.Trim(prefix, "/")
	if prefix == "" {
		return ""
	}
	return "/" + prefix
}

func (c *client) initPathConfigFromEnv() {
	c.apiPrefix = normalizePrefix(parseEnvString("F5_HTTP_API_PREFIX", ""))

	// Probe settings (optional; safe defaults).
	c.probePath = parseEnvString("F5_HTTP_PROBE_PATH", "/")
	c.probeMethod = strings.ToUpper(strings.TrimSpace(parseEnvString("F5_HTTP_PROBE_METHOD", http.MethodGet)))
	if c.probeMethod == "" {
		c.probeMethod = http.MethodGet
	}
}

// ProbeResult captures a lightweight response summary for connectivity/auth probing.
type ProbeResult struct {
	Method      string
	URL         string
	StatusCode  int
	Status      string
	RequestID   string
	BodySnippet string
}

func (c *client) Probe(ctx context.Context) (*ProbeResult, error) {
	path := c.withPrefix(c.probePath)
	url := c.endpoint + "/" + strings.TrimLeft(path, "/")

	req, err := http.NewRequestWithContext(ctx, c.probeMethod, url, nil)
	if err != nil {
		return nil, fmt.Errorf("creating probe request %s %s: %w", c.probeMethod, url, err)
	}

	req.Header.Set("Accept", "application/json")
	if c.ceAuth != "" {
		req.Header.Set("Ce-Auth", c.ceAuth)
	}
	if c.organisationName != "" {
		req.Header.Set("organisation-name", c.organisationName)
		req.Header.Set("organization-name", c.organisationName)
	}
	if c.projectID != "" {
		req.Header.Set("project-id", c.projectID)
	}
	if c.username != "" && c.password != "" {
		req.SetBasicAuth(c.username, c.password)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("performing probe %s %s: %w", c.probeMethod, url, err)
	}
	defer resp.Body.Close()

	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))

	reqID := resp.Header.Get("X-Request-Id")
	if reqID == "" {
		reqID = resp.Header.Get("X-Request-ID")
	}

	return &ProbeResult{
		Method:      c.probeMethod,
		URL:         url,
		StatusCode:  resp.StatusCode,
		Status:      resp.Status,
		RequestID:   reqID,
		BodySnippet: string(b),
	}, nil
}

// GenerateCeAuthToken produces a time-limited Ce-Auth token from long-lived API
// credentials (api_key_id + secret_key). The token format is:
//
//	{api_key_id}.{expiry_unix_timestamp}.{hmac_sha256_hex}
//
// This matches the Python implementation in scripts/cmp-ceauth.py.
// The default validity is 299 seconds (~5 minutes).
func GenerateCeAuthToken(apiKeyID, secretKey string, validity time.Duration) string {
	if validity <= 0 {
		validity = 299 * time.Second
	}
	expiryTimestamp := time.Now().Unix() + int64(validity.Seconds())
	input := fmt.Sprintf("%s.%d", apiKeyID, expiryTimestamp)
	mac := hmac.New(sha256.New, []byte(secretKey))
	mac.Write([]byte(input))
	signature := hex.EncodeToString(mac.Sum(nil))
	return fmt.Sprintf("%s.%d.%s", apiKeyID, expiryTimestamp, signature)
}

// NewClient creates a new F5 client with the given connection details.
// Username/password are read from env vars F5_USERNAME / F5_PASSWORD.
func NewClient(log logr.Logger, endpoint, partition string) (Client, error) {
	if endpoint == "" {
		return nil, fmt.Errorf("f5 endpoint must not be empty")
	}
	if partition == "" {
		return nil, fmt.Errorf("f5 partition must not be empty")
	}

	endpoint = strings.TrimRight(endpoint, "/")

	c := &client{
		log:         log.WithValues("f5Endpoint", endpoint, "f5Partition", partition),
		endpoint:    endpoint,
		partition:   partition,
		httpClient:  newHTTPClientFromEnv(),
		username:    os.Getenv("F5_USERNAME"),
		password:    os.Getenv("F5_PASSWORD"),
		rateLimiter: rate.NewLimiter(rate.Limit(10), 20),
	}
	c.initPathConfigFromEnv()

	if c.username == "" || c.password == "" {
		c.log.Info("F5 credentials not configured via F5_USERNAME/F5_PASSWORD; HTTP calls will likely fail")
	}

	return c, nil
}

// NewClientWithBasicAuth creates a new client using explicit Basic Auth credentials.
// This is preferred for controllers where credentials come from Kubernetes Secrets.
func NewClientWithBasicAuth(log logr.Logger, endpoint, partition, username, password string) (Client, error) {
	if endpoint == "" {
		return nil, fmt.Errorf("f5 endpoint must not be empty")
	}
	if partition == "" {
		return nil, fmt.Errorf("f5 partition must not be empty")
	}

	endpoint = strings.TrimRight(endpoint, "/")

	c := &client{
		log:         log.WithValues("f5Endpoint", endpoint, "f5Partition", partition),
		endpoint:    endpoint,
		partition:   partition,
		httpClient:  newHTTPClientFromEnv(),
		username:    username,
		password:    password,
		rateLimiter: rate.NewLimiter(rate.Limit(10), 20),
	}
	c.initPathConfigFromEnv()

	if c.username == "" || c.password == "" {
		c.log.Info("F5 credentials not configured; HTTP calls will likely fail")
	}

	return c, nil
}

// NewClientWithCeAuth creates a client for CMP/CCP APIs using the Ce-Auth header.
// This matches curl usage like:
//
//	-H 'Ce-Auth: <token>'
//	-H 'organisation-name: <tenant>'
//	-H 'project-id: <id>'
func NewClientWithCeAuth(log logr.Logger, endpoint, organisationName, projectID, ceAuth string) (Client, error) {
	if endpoint == "" {
		return nil, fmt.Errorf("f5 endpoint must not be empty")
	}
	if organisationName == "" {
		return nil, fmt.Errorf("organisation name must not be empty")
	}
	if projectID == "" {
		return nil, fmt.Errorf("project id must not be empty")
	}
	if ceAuth == "" {
		return nil, fmt.Errorf("Ce-Auth token must not be empty")
	}

	endpoint = strings.TrimRight(endpoint, "/")

	c := &client{
		log:              log.WithValues("f5Endpoint", endpoint, "organisationName", organisationName, "projectID", projectID),
		endpoint:         endpoint,
		partition:        organisationName,
		organisationName: organisationName,
		projectID:        projectID,
		ceAuth:           ceAuth,
		httpClient:       newHTTPClientFromEnv(),
		extraHeaders:     make(http.Header),
		rateLimiter:      rate.NewLimiter(rate.Limit(10), 20),
	}
	c.initPathConfigFromEnv()

	return c, nil
}

// EnsureControlPlaneVirtualServer provisions the control-plane LB via CMP LBaaS.
// All VIPs are allocated via CMP — no direct BIG-IP communication.
func (c *client) EnsureControlPlaneVirtualServer(ctx context.Context, vip string, port int32, backends []Backend) (*CMPResourceIDs, error) {
	if vip == "" {
		return nil, fmt.Errorf("vip must not be empty")
	}
	if port <= 0 || port > 65535 {
		return nil, fmt.Errorf("port must be between 1 and 65535")
	}

	return c.ensureControlPlaneViaCMPLBaaS(ctx, vip, port, backends)
}

// ensureControlPlaneViaCMPLBaaS implements the CMP LBaaS v2.1 flow:
// 1. Create (or reuse) LB Service
// 2. Create (or reuse) VIP port
// 3. Create (or reuse) Virtual Server with backend nodes
func (c *client) ensureControlPlaneViaCMPLBaaS(ctx context.Context, vip string, port int32, backends []Backend) (*CMPResourceIDs, error) {
	c.log.Info("Ensuring control-plane VS via CMP LBaaS v2.1 flow",
		"vip", vip, "port", port, "backendCount", len(backends))

	ids := &CMPResourceIDs{}

	// Step 1: Create LB Service if not already present.
	lbServiceID, err := c.ensureOrFindLBService(ctx)
	if err != nil {
		return nil, fmt.Errorf("ensuring LB service: %w", err)
	}
	ids.LBServiceID = lbServiceID
	c.log.Info("LB Service ready", "lbServiceID", lbServiceID)

	// Step 2: Create VIP port on the LB Service.
	vipPortID, err := c.ensureOrFindVIP(ctx, lbServiceID)
	if err != nil {
		return ids, fmt.Errorf("ensuring VIP on LB service %s: %w", lbServiceID, err)
	}
	ids.VIPPortID = vipPortID
	c.log.Info("VIP port ready", "lbServiceID", lbServiceID, "vipPortID", vipPortID)

	// Step 3: Create Virtual Server.
	vsID, err := c.ensureOrFindVirtualServer(ctx, lbServiceID, vipPortID, port, backends)
	if err != nil {
		return ids, fmt.Errorf("ensuring virtual server on LB service %s: %w", lbServiceID, err)
	}
	ids.VirtualServerID = vsID
	ids.VirtualServerName = "cp-apiserver-vs"
	c.log.Info("Virtual server ready", "lbServiceID", lbServiceID, "vsID", vsID)

	return ids, nil
}

func (c *client) ensureOrFindLBService(ctx context.Context) (string, error) {
	expectedName := "cp-lb-" + c.partition

	// Try to find an existing LB service.
	items, err := c.ListLBServices(ctx, nil)
	if err != nil {
		// Avoid creating duplicates when listing fails (e.g. transient 5xx). Let the caller retry.
		return "", err
	}

	var (
		fallbackID     string
		fallbackName   string
		fallbackStatus string
		parseableCount int
	)

	for _, item := range items {
		var svc struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Status string `json:"status"`
		}
		if json.Unmarshal(item, &svc) != nil {
			continue
		}
		svc.ID = strings.TrimSpace(svc.ID)
		svc.Name = strings.TrimSpace(svc.Name)
		if svc.ID == "" {
			continue
		}
		parseableCount++

		if fallbackID == "" {
			fallbackID, fallbackName, fallbackStatus = svc.ID, svc.Name, svc.Status
		}

		if svc.Name == expectedName {
			c.log.Info("Found existing LB Service", "id", svc.ID, "name", svc.Name, "status", svc.Status)
			return svc.ID, nil
		}
	}

	// If the API doesn't return names and there is exactly one LB service, reuse it.
	if parseableCount == 1 && fallbackID != "" {
		c.log.Info("Found single existing LB Service", "id", fallbackID, "name", fallbackName, "status", fallbackStatus)
		return fallbackID, nil
	}

	// Create a new LB Service.
	form := url.Values{}
	form.Set("name", expectedName)
	form.Set("description", "Control-plane LB for "+c.partition)
	if c.cpFlavorID != 0 {
		form.Set("flavor_id", fmt.Sprintf("%d", c.cpFlavorID))
	}
	if c.cpNetworkID != "" {
		form.Set("network_id", c.cpNetworkID)
	}
	if c.cpVPCID != "" {
		form.Set("vpc_id", c.cpVPCID)
	}
	if c.cpVPCName != "" {
		form.Set("vpc_name", c.cpVPCName)
	}

	raw, err := c.CreateLBService(ctx, form)
	if err != nil {
		return "", err
	}

	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &created); err != nil {
		return "", fmt.Errorf("parsing LB Service create response: %w", err)
	}
	created.ID = strings.TrimSpace(created.ID)
	if created.ID == "" {
		return "", fmt.Errorf("LB Service created but no ID returned: %s", string(raw))
	}
	return created.ID, nil
}

func (c *client) ensureOrFindVIP(ctx context.Context, lbServiceID string) (string, error) {
	// Check if a VIP already exists.
	vips, err := c.GetLBServiceVIPs(ctx, lbServiceID)
	if err != nil {
		// Avoid creating duplicates when listing fails (e.g. transient 5xx). Let the caller retry.
		return "", err
	}

	for _, raw := range vips {
		var vip struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(raw, &vip) == nil {
			vip.ID = strings.TrimSpace(vip.ID)
			if vip.ID != "" {
				return vip.ID, nil
			}
		}
		// Try numeric id
		var vipNumeric struct {
			ID int `json:"id"`
		}
		if json.Unmarshal(raw, &vipNumeric) == nil && vipNumeric.ID != 0 {
			return fmt.Sprintf("%d", vipNumeric.ID), nil
		}
	}

	// Create a new VIP port.
	raw, err := c.CreateLBServiceVIP(ctx, lbServiceID)
	if err != nil {
		return "", err
	}

	var created struct {
		ID int `json:"id"`
	}
	if err := json.Unmarshal(raw, &created); err != nil {
		return "", fmt.Errorf("parsing VIP create response: %w", err)
	}
	if created.ID == 0 {
		// Try string id
		var createdStr struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(raw, &createdStr) == nil {
			createdStr.ID = strings.TrimSpace(createdStr.ID)
			if createdStr.ID != "" {
				return createdStr.ID, nil
			}
		}
		return "", fmt.Errorf("VIP created but no ID returned: %s", string(raw))
	}
	return fmt.Sprintf("%d", created.ID), nil
}

func (c *client) ensureOrFindVirtualServer(ctx context.Context, lbServiceID, vipPortID string, port int32, backends []Backend) (string, error) {
	// Check if a virtual server already exists.
	vsList, err := c.ListLBVirtualServers(ctx, lbServiceID)
	if err != nil {
		// Avoid creating duplicates when listing fails (e.g. transient 5xx). Let the caller retry.
		return "", err
	}

	const expectedName = "cp-apiserver-vs"
	var (
		fallbackID     string
		fallbackName   string
		parseableCount int
	)

	for _, raw := range vsList {
		var vs struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		if json.Unmarshal(raw, &vs) != nil {
			continue
		}
		vs.ID = strings.TrimSpace(vs.ID)
		vs.Name = strings.TrimSpace(vs.Name)
		if vs.ID == "" {
			continue
		}
		parseableCount++

		if fallbackID == "" {
			fallbackID, fallbackName = vs.ID, vs.Name
		}

		if vs.Name == expectedName {
			return vs.ID, nil
		}
	}

	// If the API doesn't return names and there is exactly one VS, reuse it.
	if parseableCount == 1 && fallbackID != "" {
		c.log.Info("Found single existing virtual server", "vsID", fallbackID, "name", fallbackName)
		return fallbackID, nil
	}

	// Build the node list for the virtual server.
	nodes := make([]map[string]interface{}, 0, len(backends))
	for i, b := range backends {
		node := map[string]interface{}{
			"compute_id":      fmt.Sprintf("cp-%d", i),
			"compute_ip":      b.IP,
			"backend_port_id": i + 1,
			"port":            b.Port,
			"weight":          50,
		}
		nodes = append(nodes, node)
	}
	nodesJSON, _ := json.Marshal(nodes)

	query := url.Values{}
	query.Set("name", expectedName)
	query.Set("vip_port_id", vipPortID)
	query.Set("protocol", "TCP")
	query.Set("port", fmt.Sprintf("%d", port))
	query.Set("routing_algorithm", c.cpRoutingAlgorithm)
	query.Set("interval", fmt.Sprintf("%d", c.cpMonitorInterval))
	if c.cpVPCID != "" {
		query.Set("vpc_id", c.cpVPCID)
	}
	for _, n := range nodes {
		nodeJSON, _ := json.Marshal(n)
		query.Add("nodes", string(nodeJSON))
	}
	_ = nodesJSON // suppress unused warning if needed

	raw, err := c.CreateLBVirtualServer(ctx, lbServiceID, query)
	if err != nil {
		return "", err
	}

	var created struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &created); err != nil {
		return "", fmt.Errorf("parsing virtual server create response: %w", err)
	}
	created.ID = strings.TrimSpace(created.ID)
	created.Name = strings.TrimSpace(created.Name)
	if created.ID != "" {
		return created.ID, nil
	}
	if created.Name != "" {
		return created.Name, nil
	}
	return expectedName, nil
}

// DeleteControlPlaneVirtualServer cleans up all CMP LBaaS resources in reverse order:
// VirtualServer → VIP → LBService.
func (c *client) DeleteControlPlaneVirtualServer(ctx context.Context, ids *CMPResourceIDs) error {
	if ids == nil {
		c.log.Info("No CMP resource IDs provided; nothing to clean up")
		return nil
	}

	c.log.Info("Deleting control-plane CMP LBaaS resources",
		"lbServiceID", ids.LBServiceID,
		"vipPortID", ids.VIPPortID,
		"virtualServerID", ids.VirtualServerID,
	)

	// Step 1: Delete Virtual Server.
	if ids.VirtualServerID != "" && ids.LBServiceID != "" {
		if err := c.DeleteLBVirtualServer(ctx, ids.LBServiceID, ids.VirtualServerID); err != nil {
			c.log.Error(err, "Failed to delete virtual server (best-effort)", "vsID", ids.VirtualServerID)
		}
	}

	// Step 2: Delete VIP port.
	if ids.VIPPortID != "" && ids.LBServiceID != "" {
		if err := c.DeleteLBServiceVIP(ctx, ids.LBServiceID, ids.VIPPortID); err != nil {
			c.log.Error(err, "Failed to delete VIP port (best-effort)", "vipPortID", ids.VIPPortID)
		}
	}

	// Step 3: Delete LB Service.
	if ids.LBServiceID != "" {
		if err := c.DeleteLBService(ctx, ids.LBServiceID); err != nil {
			c.log.Error(err, "Failed to delete LB service (best-effort)", "lbServiceID", ids.LBServiceID)
		}
	}

	return nil
}

// ListLoadBalancers calls GET /load-balancers/.
//
// Response schema depends on CMP; we return the raw JSON items to keep the
// controller decoupled from CMP's evolving fields.
func (c *client) ListLoadBalancers(ctx context.Context, opts *ListLoadBalancersOptions) ([]json.RawMessage, error) {
	q := url.Values{}
	if opts != nil {
		if opts.Limit != nil {
			q.Set("limit", fmt.Sprintf("%d", *opts.Limit))
		}
		if opts.Offset != nil {
			q.Set("offset", fmt.Sprintf("%d", *opts.Offset))
		}
		if strings.TrimSpace(opts.Field) != "" {
			q.Set("field", strings.TrimSpace(opts.Field))
		}
		if strings.TrimSpace(opts.Order) != "" {
			q.Set("order", strings.TrimSpace(opts.Order))
		}
		if strings.TrimSpace(opts.Search) != "" {
			q.Set("search", strings.TrimSpace(opts.Search))
		}
	}

	path := "/"
	if enc := q.Encode(); enc != "" {
		path += "?" + enc
	}

	var out []json.RawMessage
	if err := c.doRequest(ctx, http.MethodGet, c.lbPath(path), nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// CreateLoadBalancer calls POST /load-balancers/.
//
// The CMP Swagger uses formData for many operations; we accept a generic form map
// and return the raw JSON response to avoid pinning to a specific schema.
func (c *client) CreateLoadBalancer(ctx context.Context, form url.Values) (json.RawMessage, error) {
	var out json.RawMessage
	if err := c.doFormRequest(ctx, http.MethodPost, c.lbPath("/"), form, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListLBServices calls GET /load-balancers/lb_service/.
func (c *client) ListLBServices(ctx context.Context, opts *ListLoadBalancersOptions) ([]json.RawMessage, error) {
	q := url.Values{}
	if opts != nil {
		if opts.Limit != nil {
			q.Set("limit", fmt.Sprintf("%d", *opts.Limit))
		}
		if opts.Offset != nil {
			q.Set("offset", fmt.Sprintf("%d", *opts.Offset))
		}
		if strings.TrimSpace(opts.Field) != "" {
			q.Set("field", strings.TrimSpace(opts.Field))
		}
		if strings.TrimSpace(opts.Order) != "" {
			q.Set("order", strings.TrimSpace(opts.Order))
		}
		if strings.TrimSpace(opts.Search) != "" {
			q.Set("search", strings.TrimSpace(opts.Search))
		}
	}

	path := "/lb_service/"
	if enc := q.Encode(); enc != "" {
		path += "?" + enc
	}

	var out []json.RawMessage
	if err := c.doRequest(ctx, http.MethodGet, c.lbPath(path), nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// CreateLBService calls POST /load-balancers/lb_service/.
func (c *client) CreateLBService(ctx context.Context, form url.Values) (json.RawMessage, error) {
	var out json.RawMessage
	if err := c.doFormRequest(ctx, http.MethodPost, c.lbPath("/lb_service/"), form, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetLBService calls GET /load-balancers/lb_service/{id}/.
func (c *client) GetLBService(ctx context.Context, id string) (json.RawMessage, error) {
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("lb service id must not be empty")
	}

	var out json.RawMessage
	if err := c.doRequest(ctx, http.MethodGet, c.lbPath("/lb_service/"+strings.TrimSpace(id)+"/"), nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteLBService calls DELETE /load-balancers/lb_service/{id}/.
func (c *client) DeleteLBService(ctx context.Context, id string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("lb service id must not be empty")
	}
	err := c.doRequest(ctx, http.MethodDelete, c.lbPath("/lb_service/"+strings.TrimSpace(id)+"/"), nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// GetLoadBalancer calls GET /load-balancers/{id}/.
func (c *client) GetLoadBalancer(ctx context.Context, id string) (json.RawMessage, error) {
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("load balancer id must not be empty")
	}

	var out json.RawMessage
	if err := c.doRequest(ctx, http.MethodGet, c.lbPath("/"+strings.TrimSpace(id)+"/"), nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// UpdateLoadBalancer calls PATCH /load-balancers/{id}/.
//
// The CMP Swagger frequently models updates as formData; we keep it generic.
func (c *client) UpdateLoadBalancer(ctx context.Context, id string, form url.Values) (json.RawMessage, error) {
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("load balancer id must not be empty")
	}

	var out json.RawMessage
	if err := c.doFormRequest(ctx, http.MethodPatch, c.lbPath("/"+strings.TrimSpace(id)+"/"), form, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteLoadBalancer calls DELETE /load-balancers/{id}/.
func (c *client) DeleteLoadBalancer(ctx context.Context, id string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("load balancer id must not be empty")
	}

	err := c.doRequest(ctx, http.MethodDelete, c.lbPath("/"+strings.TrimSpace(id)+"/"), nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// OptionsLoadBalancers calls OPTIONS /load-balancers/ and returns the Allow header.
func (c *client) OptionsLoadBalancers(ctx context.Context) ([]string, error) {
	h, err := c.doRequestWithBody(ctx, http.MethodOptions, c.lbPath("/"), nil, "", nil)
	if err != nil {
		return nil, err
	}

	allow := h.Get("Allow")
	if allow == "" {
		allow = h.Get("ALLOW")
	}
	if strings.TrimSpace(allow) == "" {
		return nil, nil
	}

	parts := strings.Split(allow, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		m := strings.TrimSpace(p)
		if m != "" {
			out = append(out, m)
		}
	}
	return out, nil
}

// CreateLBServiceVIP calls POST /load-balancers/lb_service/{id}/vip.
func (c *client) CreateLBServiceVIP(ctx context.Context, lbServiceID string) (json.RawMessage, error) {
	lbServiceID = strings.TrimSpace(lbServiceID)
	if lbServiceID == "" {
		return nil, fmt.Errorf("lb service id must not be empty")
	}
	var out json.RawMessage
	if err := c.doRequest(ctx, http.MethodPost, c.lbPath("/lb_service/"+lbServiceID+"/vip"), nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetLBServiceVIPs calls GET /load-balancers/lb_service/{id}/vip.
func (c *client) GetLBServiceVIPs(ctx context.Context, lbServiceID string) ([]json.RawMessage, error) {
	lbServiceID = strings.TrimSpace(lbServiceID)
	if lbServiceID == "" {
		return nil, fmt.Errorf("lb service id must not be empty")
	}
	var out []json.RawMessage
	if err := c.doRequest(ctx, http.MethodGet, c.lbPath("/lb_service/"+lbServiceID+"/vip"), nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteLBServiceVIP calls DELETE /load-balancers/lb_service/{id}/vip/{vipID}/.
func (c *client) DeleteLBServiceVIP(ctx context.Context, lbServiceID, vipID string) error {
	lbServiceID = strings.TrimSpace(lbServiceID)
	vipID = strings.TrimSpace(vipID)
	if lbServiceID == "" {
		return fmt.Errorf("lb service id must not be empty")
	}
	if vipID == "" {
		return fmt.Errorf("vip id must not be empty")
	}
	err := c.doRequest(ctx, http.MethodDelete, c.lbPath("/lb_service/"+lbServiceID+"/vip/"+vipID+"/"), nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// ListLBVirtualServers calls GET /load-balancers/{lbServiceID}/virtual-servers.
func (c *client) ListLBVirtualServers(ctx context.Context, lbServiceID string) ([]json.RawMessage, error) {
	lbServiceID = strings.TrimSpace(lbServiceID)
	if lbServiceID == "" {
		return nil, fmt.Errorf("lb service id must not be empty")
	}
	var out []json.RawMessage
	if err := c.doRequest(ctx, http.MethodGet, c.lbPath("/"+lbServiceID+"/virtual-servers"), nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// CreateLBVirtualServer calls POST /load-balancers/{lbServiceID}/virtual-servers.
// Per the CMP Swagger v2.1, parameters are query params.
func (c *client) CreateLBVirtualServer(ctx context.Context, lbServiceID string, query url.Values) (json.RawMessage, error) {
	lbServiceID = strings.TrimSpace(lbServiceID)
	if lbServiceID == "" {
		return nil, fmt.Errorf("lb service id must not be empty")
	}
	path := "/" + lbServiceID + "/virtual-servers"
	if query != nil {
		if enc := query.Encode(); enc != "" {
			path += "?" + enc
		}
	}
	var out json.RawMessage
	if err := c.doRequest(ctx, http.MethodPost, c.lbPath(path), nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetLBVirtualServer calls GET /load-balancers/{lbServiceID}/virtual-servers/{vsID}.
func (c *client) GetLBVirtualServer(ctx context.Context, lbServiceID, vsID string) (json.RawMessage, error) {
	lbServiceID = strings.TrimSpace(lbServiceID)
	vsID = strings.TrimSpace(vsID)
	if lbServiceID == "" {
		return nil, fmt.Errorf("lb service id must not be empty")
	}
	if vsID == "" {
		return nil, fmt.Errorf("virtual server id must not be empty")
	}
	var out json.RawMessage
	if err := c.doRequest(ctx, http.MethodGet, c.lbPath("/"+lbServiceID+"/virtual-servers/"+vsID), nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteLBVirtualServer calls DELETE /load-balancers/{lbServiceID}/virtual-servers/{vsID}.
func (c *client) DeleteLBVirtualServer(ctx context.Context, lbServiceID, vsID string) error {
	lbServiceID = strings.TrimSpace(lbServiceID)
	vsID = strings.TrimSpace(vsID)
	if lbServiceID == "" {
		return fmt.Errorf("lb service id must not be empty")
	}
	if vsID == "" {
		return fmt.Errorf("virtual server id must not be empty")
	}
	err := c.doRequest(ctx, http.MethodDelete, c.lbPath("/"+lbServiceID+"/virtual-servers/"+vsID), nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// SearchNetworkPortsByIP calls GET /networks/ports/search-by-ip/?fixed_ip={ip}.
func (c *client) SearchNetworkPortsByIP(ctx context.Context, ip string) ([]json.RawMessage, error) {
	ip = strings.TrimSpace(ip)
	if ip == "" {
		return nil, fmt.Errorf("ip must not be empty")
	}
	q := url.Values{}
	q.Set("fixed_ip", ip)
	var out []json.RawMessage
	if err := c.doRequest(ctx, http.MethodGet, c.networkPath("/ports/search-by-ip/?"+q.Encode()), nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListLBFlavors calls GET /load-balancers/lb-flavor/.
func (c *client) ListLBFlavors(ctx context.Context) ([]json.RawMessage, error) {
	var out []json.RawMessage
	if err := c.doRequest(ctx, http.MethodGet, c.lbPath("/lb-flavor/"), nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ---- HTTP helper types ----

func (c *client) withPrefix(path string) string {
	path = "/" + strings.TrimLeft(path, "/")
	if c.apiPrefix == "" {
		return path
	}
	return c.apiPrefix + path
}

// lbPath builds a CMP LBaaS v2.1 URL path with domain/{org}/project/{proj} scoping.
// If organisationName or projectID are empty, falls back to the legacy prefix-based path.
func (c *client) lbPath(subPath string) string {
	if c.organisationName != "" && c.projectID != "" {
		return fmt.Sprintf("/api/v2.1/load-balancers/domain/%s/project/%s/load-balancers%s",
			url.PathEscape(c.organisationName),
			url.PathEscape(c.projectID),
			subPath)
	}
	return c.withPrefix("/load-balancers" + subPath)
}

func (c *client) networkPath(subPath string) string {
	if c.organisationName != "" && c.projectID != "" {
		return fmt.Sprintf("/api/v2.1/networks/domain/%s/project/%s/networks%s",
			url.PathEscape(c.organisationName),
			url.PathEscape(c.projectID),
			subPath)
	}
	return c.withPrefix("/networks" + subPath)
}

// doRequest is a small helper for JSON-based HTTP requests.
func (c *client) doRequest(ctx context.Context, method, path string, body any, result any) error {
	var reader io.Reader
	contentType := ""
	if body != nil {
		b := &bytes.Buffer{}
		if err := json.NewEncoder(b).Encode(body); err != nil {
			return fmt.Errorf("encoding request body: %w", err)
		}
		reader = b
		contentType = "application/json"
	}

	_, err := c.doRequestWithBody(ctx, method, path, reader, contentType, result)
	return err
}

func (c *client) doFormRequest(ctx context.Context, method, path string, form url.Values, result any) error {
	var reader io.Reader
	if form != nil {
		reader = strings.NewReader(form.Encode())
	} else {
		reader = strings.NewReader("")
	}
	_, err := c.doRequestWithBody(ctx, method, path, reader, "application/x-www-form-urlencoded", result)
	return err
}

func (c *client) doRequestWithBody(ctx context.Context, method, path string, body io.Reader, contentType string, result any) (http.Header, error) {
	// Client-side rate limiting: wait for a token before sending the request.
	// This prevents bursting all Shoot reconciliations at once hitting CMP's rate limit.
	if err := c.rateLimiter.Wait(ctx); err != nil {
		return nil, fmt.Errorf("rate limiter wait cancelled: %w", err)
	}

	url := c.endpoint + "/" + strings.TrimLeft(path, "/")

	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, fmt.Errorf("creating %s %s request: %w", method, url, err)
	}

	// Default headers
	req.Header.Set("Accept", "application/json")

	// Optional extra headers (debug only / non-standard CMP requirements)
	for k, vs := range c.extraHeaders {
		for _, v := range vs {
			if strings.TrimSpace(k) != "" && v != "" {
				req.Header.Add(k, v)
			}
		}
	}
	if c.ceAuth != "" {
		req.Header.Set("Ce-Auth", c.ceAuth)
	}
	if c.organisationName != "" {
		// Some environments use US spelling; set both for compatibility.
		req.Header.Set("organisation-name", c.organisationName)
		req.Header.Set("organization-name", c.organisationName)
	}
	if c.projectID != "" {
		req.Header.Set("project-id", c.projectID)
	}

	if strings.TrimSpace(contentType) != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if c.username != "" && c.password != "" {
		req.SetBasicAuth(c.username, c.password)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("performing %s %s: %w", method, url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))

		// Include a few helpful headers (non-sensitive) to ease debugging 401/403 issues.
		reqID := resp.Header.Get("X-Request-Id")
		if reqID == "" {
			reqID = resp.Header.Get("X-Request-ID")
		}
		wwwAuth := resp.Header.Get("Www-Authenticate")
		if wwwAuth == "" {
			wwwAuth = resp.Header.Get("WWW-Authenticate")
		}

		// Handle 429 Too Many Requests: parse Retry-After and return a typed error
		// so callers (controller-runtime reconcilers) can requeue after the correct delay.
		if resp.StatusCode == http.StatusTooManyRequests {
			retryAfter := 30 * time.Second // default retry delay
			if ra := resp.Header.Get("Retry-After"); ra != "" {
				if secs, err := strconv.Atoi(ra); err == nil && secs > 0 {
					retryAfter = time.Duration(secs) * time.Second
				}
			}
			return nil, &RateLimitedError{
				RetryAfter: retryAfter,
				Message:    fmt.Sprintf("%s %s: %s (reqID=%s)", method, url, string(b), reqID),
			}
		}

		return nil, &HTTPStatusError{
			Method:          method,
			URL:             url,
			StatusCode:      resp.StatusCode,
			Status:          resp.Status,
			RequestID:       reqID,
			WWWAuthenticate: wwwAuth,
			BodySnippet:     string(b),
		}
	}

	if result != nil {
		if err := json.NewDecoder(resp.Body).Decode(result); err != nil && err != io.EOF {
			return nil, fmt.Errorf("decoding response from %s %s: %w", method, url, err)
		}
	}

	return resp.Header.Clone(), nil
}
