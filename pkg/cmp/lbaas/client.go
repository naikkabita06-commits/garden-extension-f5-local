package lbaas

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client is a minimal CMP LBaaS client for LBService/VIP/VirtualServer APIs.
//
// It intentionally returns raw JSON to avoid pinning to CMP's evolving schemas.
// This matches the style used in pkg/f5/client.go and mirrors the "thin client"
// approach from the TCPWave DNS integration.
//
// Auth:
// - Ce-Auth (HMAC) is supported for environments where LBaaS endpoints accept it.
//
// IMPORTANT: This client does not try to follow 30x redirects because some CMP
// deployments redirect to internal hostnames (e.g. neutron-service) not resolvable
// from outside the cluster.
type Client struct {
	endpoint string
	hc       *http.Client
	now      func() time.Time

	orgName   string
	projectID string

	// UI-scoping headers commonly required by Airtel CMP.
	region          string
	externalProject string
	projectName     string
	username        string

	// Auth mode
	ceAuthToken  string
	ceAuthID     string
	ceAuthSecret string
	ceAuthExpiry time.Duration
}

// Options configures the LBaaS client.
type Options struct {
	HTTPClient *http.Client

	OrganisationName string
	ProjectID        string

	Region          string // header: ce-region
	ExternalProject string // header: external-project
	ProjectName     string // header: project-name
	Username        string // header: username
	CeAuthToken     string // Use this Ce-Auth token as-is

	CeAuthAPIID  string // used to generate Ce-Auth
	CeAuthSecret string
	CeAuthExpiry time.Duration // defaults to 299s
}

func New(endpoint string, opts Options) (*Client, error) {
	endpoint = strings.TrimRight(strings.TrimSpace(endpoint), "/")
	if endpoint == "" {
		return nil, fmt.Errorf("endpoint must not be empty")
	}

	hc := opts.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}

	exp := opts.CeAuthExpiry
	if exp == 0 {
		exp = 299 * time.Second
	}

	return &Client{
		endpoint:        endpoint,
		hc:              hc,
		orgName:         strings.TrimSpace(opts.OrganisationName),
		projectID:       strings.TrimSpace(opts.ProjectID),
		now:             time.Now,
		region:          strings.TrimSpace(opts.Region),
		externalProject: strings.TrimSpace(opts.ExternalProject),
		projectName:     strings.TrimSpace(opts.ProjectName),
		username:        strings.TrimSpace(opts.Username),
		ceAuthToken:     strings.TrimSpace(opts.CeAuthToken),
		ceAuthID:        strings.TrimSpace(opts.CeAuthAPIID),
		ceAuthSecret:    strings.TrimSpace(opts.CeAuthSecret),
		ceAuthExpiry:    exp,
	}, nil
}

type ListOptions struct {
	Offset int
	Limit  int
	Search string
	Field  string
	Order  string
}

// toQueryAll matches CMP UI-style list calls: includes offset/limit/search even when empty.
func (o ListOptions) toQueryAll() string {
	q := url.Values{}
	q.Set("offset", fmt.Sprintf("%d", o.Offset))
	limit := o.Limit
	if limit <= 0 {
		limit = 10
	}
	q.Set("limit", fmt.Sprintf("%d", limit))
	q.Set("search", o.Search)
	if strings.TrimSpace(o.Field) != "" {
		q.Set("field", strings.TrimSpace(o.Field))
	}
	if strings.TrimSpace(o.Order) != "" {
		q.Set("order", strings.TrimSpace(o.Order))
	}
	return "?" + q.Encode()
}

// toQueryNonEmpty is a more standard list query builder: it only includes fields that are set.
func (o ListOptions) toQueryNonEmpty() string {
	q := url.Values{}
	if o.Offset > 0 {
		q.Set("offset", fmt.Sprintf("%d", o.Offset))
	}
	if o.Limit > 0 {
		q.Set("limit", fmt.Sprintf("%d", o.Limit))
	}
	if strings.TrimSpace(o.Search) != "" {
		q.Set("search", strings.TrimSpace(o.Search))
	}
	if strings.TrimSpace(o.Field) != "" {
		q.Set("field", strings.TrimSpace(o.Field))
	}
	if strings.TrimSpace(o.Order) != "" {
		q.Set("order", strings.TrimSpace(o.Order))
	}
	enc := q.Encode()
	if enc == "" {
		return ""
	}
	return "?" + enc
}

// lbPath builds a CMP LBaaS v2.1 URL path with domain/{org}/project/{proj} scoping.
func (c *Client) lbPath(subPath string) string {
	if c.orgName != "" && c.projectID != "" {
		return fmt.Sprintf("/api/v2.1/load-balancers/domain/%s/project/%s/load-balancers%s",
			url.PathEscape(c.orgName),
			url.PathEscape(c.projectID),
			subPath)
	}
	return "/api/v2.1/load-balancers" + subPath
}

// ListLoadBalancers calls GET /api/v2.1/load-balancers/domain/{org}/project/{proj}/load-balancers/.
func (c *Client) ListLoadBalancers(ctx context.Context, opts ListOptions) ([]json.RawMessage, error) {
	path := c.lbPath("/" + opts.toQueryNonEmpty())
	var out []json.RawMessage
	if err := c.do(ctx, http.MethodGet, path, nil, "", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListLBServices calls GET /api/v2.1/load-balancers/domain/{org}/project/{proj}/load-balancers/lb_service/.
func (c *Client) ListLBServices(ctx context.Context, opts ListOptions) ([]json.RawMessage, error) {
	path := c.lbPath("/lb_service/" + opts.toQueryAll())
	var out []json.RawMessage
	if err := c.do(ctx, http.MethodGet, path, nil, "", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// CreateLBService calls POST /api/v2.1/load-balancers/domain/{org}/project/{proj}/load-balancers/lb_service/.
func (c *Client) CreateLBService(ctx context.Context, form url.Values) (json.RawMessage, error) {
	var out json.RawMessage
	if err := c.doForm(ctx, http.MethodPost, c.lbPath("/lb_service/"), form, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetLBServiceVIPs calls GET /api/v2.1/load-balancers/domain/{org}/project/{proj}/load-balancers/lb_service/{id}/vip.
func (c *Client) GetLBServiceVIPs(ctx context.Context, lbServiceID string) (json.RawMessage, error) {
	lbServiceID = strings.TrimSpace(lbServiceID)
	if lbServiceID == "" {
		return nil, fmt.Errorf("lbServiceID must not be empty")
	}
	var out json.RawMessage
	path := c.lbPath("/lb_service/" + lbServiceID + "/vip")
	if err := c.do(ctx, http.MethodGet, path, nil, "", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// CreateLBServiceVIP calls POST /api/v2.1/load-balancers/domain/{org}/project/{proj}/load-balancers/lb_service/{id}/vip.
func (c *Client) CreateLBServiceVIP(ctx context.Context, lbServiceID string, _ url.Values) (json.RawMessage, error) {
	lbServiceID = strings.TrimSpace(lbServiceID)
	if lbServiceID == "" {
		return nil, fmt.Errorf("lbServiceID must not be empty")
	}
	var out json.RawMessage
	path := c.lbPath("/lb_service/" + lbServiceID + "/vip")
	if err := c.do(ctx, http.MethodPost, path, nil, "", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListVirtualServers calls GET /api/v2.1/load-balancers/domain/{org}/project/{proj}/load-balancers/{lbID}/virtual-servers.
func (c *Client) ListVirtualServers(ctx context.Context, lbID string) ([]json.RawMessage, error) {
	lbID = strings.TrimSpace(lbID)
	if lbID == "" {
		return nil, fmt.Errorf("lbID must not be empty")
	}
	var out []json.RawMessage
	path := c.lbPath("/" + lbID + "/virtual-servers")
	if err := c.do(ctx, http.MethodGet, path, nil, "", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// CreateVirtualServer calls POST /api/v2.1/load-balancers/domain/{org}/project/{proj}/load-balancers/{lbID}/virtual-servers.
//
// Per CMP Swagger v2.1, parameters are query params.
func (c *Client) CreateVirtualServer(ctx context.Context, lbID string, query url.Values) (json.RawMessage, error) {
	lbID = strings.TrimSpace(lbID)
	if lbID == "" {
		return nil, fmt.Errorf("lbID must not be empty")
	}
	var out json.RawMessage
	subPath := "/" + lbID + "/virtual-servers"
	if query != nil {
		if enc := query.Encode(); enc != "" {
			subPath += "?" + enc
		}
	}
	path := c.lbPath(subPath)
	if err := c.do(ctx, http.MethodPost, path, nil, "", &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) doForm(ctx context.Context, method, path string, form url.Values, out any) error {
	body := strings.NewReader("")
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	return c.do(ctx, method, path, body, "application/x-www-form-urlencoded", out)
}

func (c *Client) do(ctx context.Context, method, path string, body io.Reader, contentType string, out any) error {
	path = "/" + strings.TrimLeft(path, "/")
	urlStr := c.endpoint + path

	req, err := http.NewRequestWithContext(ctx, method, urlStr, body)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}

	req.Header.Set("Accept", "application/json")
	if strings.TrimSpace(contentType) != "" {
		req.Header.Set("Content-Type", contentType)
	}

	// Auth: Ce-Auth.
	if c.ceAuthToken != "" {
		req.Header.Set("Ce-Auth", c.ceAuthToken)
	} else if c.ceAuthID != "" && c.ceAuthSecret != "" {
		req.Header.Set("Ce-Auth", c.generateCeAuth())
	}

	// Required tenancy headers
	if c.orgName != "" {
		req.Header.Set("organisation-name", c.orgName)
		req.Header.Set("organization-name", c.orgName)
	}
	if c.projectID != "" {
		req.Header.Set("project-id", c.projectID)
	}

	// Optional UI scoping headers
	if c.region != "" {
		req.Header.Set("ce-region", c.region)
	}
	if c.externalProject != "" {
		req.Header.Set("external-project", c.externalProject)
	}
	if c.projectName != "" {
		req.Header.Set("project-name", c.projectName)
	}
	if c.username != "" {
		req.Header.Set("username", c.username)
	}

	// Never follow redirects: CMP sometimes redirects to internal hostnames that
	// are not resolvable from within the extension controller.
	hc := c.hc
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	localClient := *hc
	localClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}

	resp, err := localClient.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s failed: %w", method, urlStr, err)
	}
	defer resp.Body.Close()

	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: status %d: %s", method, urlStr, resp.StatusCode, strings.TrimSpace(string(b)))
	}

	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			// Some CMP endpoints respond with primitive JSON or empty body; tolerate empty.
			if len(strings.TrimSpace(string(b))) == 0 {
				return nil
			}
			return fmt.Errorf("decoding response: %w", err)
		}
	}
	return nil
}

func (c *Client) generateCeAuth() string {
	// Token format: api_id.expiry_timestamp.hex(hmac_sha256(secret, api_id.expiry_timestamp))
	now := time.Now
	if c.now != nil {
		now = c.now
	}
	expiryTs := now().Add(c.ceAuthExpiry).Unix()
	msg := fmt.Sprintf("%s.%d", c.ceAuthID, expiryTs)
	h := hmac.New(sha256.New, []byte(c.ceAuthSecret))
	_, _ = h.Write([]byte(msg))
	sig := hex.EncodeToString(h.Sum(nil))
	return fmt.Sprintf("%s.%d.%s", c.ceAuthID, expiryTs, sig)
}
