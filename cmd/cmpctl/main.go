// SPDX-FileCopyrightText: 2025 SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type multiFlag []string

func (m *multiFlag) String() string {
	return strings.Join(*m, ",")
}

func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "ce-auth":
		os.Exit(runCeAuth(os.Args[2:]))
	case "lb-list":
		os.Exit(runLoadBalancerList(os.Args[2:]))
	case "lb-create":
		os.Exit(runLoadBalancerCreate(os.Args[2:]))
	case "health":
		os.Exit(runHealth(os.Args[2:]))
	case "request":
		os.Exit(runRequest(os.Args[2:]))
	case "lbsvc-create":
		os.Exit(runLBServiceCreate(os.Args[2:]))
	case "lbsvc-list":
		os.Exit(runLBServiceList(os.Args[2:]))
	case "lbsvc-get":
		os.Exit(runLBServiceGet(os.Args[2:]))
	case "lbsvc-vip":
		os.Exit(runLBServiceVIPList(os.Args[2:]))
	case "vip-create":
		os.Exit(runLBServiceVIPCreate(os.Args[2:]))
	case "vs-list":
		os.Exit(runVirtualServerList(os.Args[2:]))
	case "vs-create":
		os.Exit(runVirtualServerCreate(os.Args[2:]))
	case "-h", "--help", "help":
		usage()
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, "%s", `cmpctl: tiny CMP/CCP HTTP debug client

Usage:
	# Generate a Ce-Auth token from API id + secret (prints token to stdout)
	cmpctl ce-auth --api-id <uuid> --secret <secret> --expiry-seconds 299

	# List load balancers (if your CMP exposes this endpoint)
	cmpctl lb-list --endpoint https://cmp.example \
		--ce-auth TOKEN --organisation-name TENANT --project-id PROJECT \
		--field id --order desc

	# Create load balancer (if your CMP exposes this endpoint)
	cmpctl lb-create --endpoint https://cmp.example \
		--ce-auth TOKEN --organisation-name TENANT --project-id PROJECT \
		--name smoke-lb --description "smoke test"

  cmpctl health --endpoint https://cmp.example --path /ccp/lbaas/v1/health \
    --ce-auth TOKEN --organisation-name TENANT --project-id PROJECT

  cmpctl request --endpoint https://cmp.example --method GET --path /ccp/lbaas/v1/virtualservers \
    --ce-auth TOKEN --organisation-name TENANT --project-id PROJECT

	# LBService create (form-encoded; some CMP deployments still expect this)
	cmpctl lbsvc-create --endpoint https://cmp.example \
		--ce-auth TOKEN --organisation-name TENANT --project-id PROJECT \
		--header 'ce-region: dev' --header 'external-project: qa-first-cell' --header 'project-name: qa-first-cell' \
		--name lb-$(date +%Y%m%d%H%M%S) --flavor-id 25 --vpc-id <uuid> --network-id <uuid> --vpc-name <name>

	# List LB services
	cmpctl lbsvc-list --endpoint https://cmp.example \
		--ce-auth TOKEN --organisation-name TENANT --project-id PROJECT \
		--header 'ce-region: dev' --header 'external-project: qa-first-cell' --header 'project-name: qa-first-cell' \
		--offset 0 --limit 10 --search '' --field created --order desc

	# List VIPs for a LB service
	cmpctl lbsvc-vip --endpoint https://cmp.example \
		--ce-auth TOKEN --organisation-name TENANT --project-id PROJECT \
		--header 'ce-region: dev' --header 'external-project: qa-first-cell' --header 'project-name: qa-first-cell' \
		--lbsvc-id <lb-service-uuid-or-id>

	# Create VIP for a LB service (swagger-mode: query params)
	cmpctl vip-create --endpoint https://cmp.example \
		--ce-auth TOKEN --organisation-name TENANT --project-id PROJECT \
		--lbsvc-id <lb-service-uuid-or-id> --vip 10.0.0.10 --port 6443

	# List Virtual Servers under a LB
	cmpctl vs-list --endpoint https://cmp.example \
		--ce-auth TOKEN --organisation-name TENANT --project-id PROJECT \
		--header 'ce-region: dev' --header 'external-project: qa-first-cell' --header 'project-name: qa-first-cell' \
		--lb-id <lb-uuid-or-id>

	# Create Virtual Server (swagger-mode: query params)
	# Provide parameters via repeated --param key=value flags.
	cmpctl vs-create --endpoint https://cmp.example \
		--ce-auth TOKEN --organisation-name TENANT --project-id PROJECT \
		--header 'ce-region: dev' --header 'external-project: qa-first-cell' --header 'project-name: qa-first-cell' \
		--lb-id <lb-uuid-or-id> \
		--param name=my-vs --param description=test --param protocol=tcp

	cmpctl lbsvc-get --endpoint https://cmp.example --ce-auth TOKEN --organisation-name TENANT --project-id PROJECT --id <lbsvc-id>

Auth:
	Provide either Ce-Auth auth (recommended), OR basic auth.

Exit codes:
  0: success (2xx)
  1: request failed (non-2xx or transport error)
  2: usage error
`)
}

func runCeAuth(args []string) int {
	fs := flag.NewFlagSet("ce-auth", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	apiID := ""
	secret := ""
	expirySeconds := int64(299)
	fs.StringVar(&apiID, "api-id", "", "API id/uuid")
	fs.StringVar(&secret, "secret", "", "API secret")
	fs.Int64Var(&expirySeconds, "expiry-seconds", expirySeconds, "Expiry seconds from now")

	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	apiID = strings.TrimSpace(apiID)
	if apiID == "" {
		fmt.Fprintln(os.Stderr, "--api-id is required")
		return 2
	}
	if secret == "" {
		fmt.Fprintln(os.Stderr, "--secret is required")
		return 2
	}
	if expirySeconds <= 0 {
		fmt.Fprintln(os.Stderr, "--expiry-seconds must be > 0")
		return 2
	}

	expiryTs := time.Now().Add(time.Duration(expirySeconds) * time.Second).Unix()
	msg := fmt.Sprintf("%s.%d", apiID, expiryTs)
	h := hmac.New(sha256.New, []byte(secret))
	_, _ = h.Write([]byte(msg))
	sig := hex.EncodeToString(h.Sum(nil))
	token := fmt.Sprintf("%s.%d.%s", apiID, expiryTs, sig)
	fmt.Fprintln(os.Stdout, token)
	return 0
}

type listFlags struct {
	offset int
	limit  int
	search string
	field  string
	order  string
}

func (f *listFlags) toQueryNonEmpty() string {
	q := url.Values{}
	if f.offset > 0 {
		q.Set("offset", strconv.Itoa(f.offset))
	}
	if f.limit > 0 {
		q.Set("limit", strconv.Itoa(f.limit))
	}
	if strings.TrimSpace(f.search) != "" {
		q.Set("search", strings.TrimSpace(f.search))
	}
	if strings.TrimSpace(f.field) != "" {
		q.Set("field", strings.TrimSpace(f.field))
	}
	if strings.TrimSpace(f.order) != "" {
		q.Set("order", strings.TrimSpace(f.order))
	}
	enc := q.Encode()
	if enc == "" {
		return ""
	}
	return "?" + enc
}

func runLoadBalancerList(args []string) int {
	fs := flag.NewFlagSet("lb-list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var cf commonFlags
	cf.add(fs)

	field := ""
	order := ""
	search := ""
	limit := 0
	offset := 0
	fs.StringVar(&field, "field", "", "Sort field")
	fs.StringVar(&order, "order", "", "Sort order (asc|desc)")
	fs.StringVar(&search, "search", "", "Search query")
	fs.IntVar(&limit, "limit", 0, "Pagination limit")
	fs.IntVar(&offset, "offset", 0, "Pagination offset")

	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if strings.TrimSpace(cf.path) == "" {
		lf := listFlags{offset: offset, limit: limit, search: search, field: field, order: order}
		cf.path = "/api/v2.1/load-balancers/domain/" + url.PathEscape(cf.orgName) + "/project/" + url.PathEscape(cf.projectID) + "/load-balancers/" + lf.toQueryNonEmpty()
	}
	if err := cf.validate(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}

	code, err := do(cf, http.MethodGet, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if code < 200 || code >= 300 {
		return 1
	}
	return 0
}

func runLoadBalancerCreate(args []string) int {
	fs := flag.NewFlagSet("lb-create", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var cf commonFlags
	cf.add(fs)

	name := ""
	desc := ""
	fs.StringVar(&name, "name", "", "Load balancer name")
	fs.StringVar(&desc, "description", "", "Description")

	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if strings.TrimSpace(name) == "" {
		fmt.Fprintln(os.Stderr, "--name is required")
		return 2
	}
	if strings.TrimSpace(cf.path) == "" {
		cf.path = "/api/v2.1/load-balancers/domain/" + url.PathEscape(cf.orgName) + "/project/" + url.PathEscape(cf.projectID) + "/load-balancers/"
	}
	if err := cf.validate(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}

	form := url.Values{}
	form.Set("name", name)
	form.Set("description", desc)
	cf.headers = append(cf.headers, "Content-Type: application/x-www-form-urlencoded")

	code, err := do(cf, http.MethodPost, strings.NewReader(form.Encode()))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if code < 200 || code >= 300 {
		return 1
	}
	return 0
}

func (f *listFlags) add(fs *flag.FlagSet) {
	fs.IntVar(&f.offset, "offset", 0, "Pagination offset")
	fs.IntVar(&f.limit, "limit", 10, "Pagination limit")
	fs.StringVar(&f.search, "search", "", "Search query")
	fs.StringVar(&f.field, "field", "", "Sort field")
	fs.StringVar(&f.order, "order", "", "Sort order (asc|desc)")
}

func (f *listFlags) toQuery() string {
	q := url.Values{}
	// Match UI requests: include offset/limit even when 0/default.
	q.Set("offset", strconv.Itoa(f.offset))
	q.Set("limit", strconv.Itoa(f.limit))
	// Match UI requests: include search even when empty (search=).
	q.Set("search", f.search)
	if strings.TrimSpace(f.field) != "" {
		q.Set("field", strings.TrimSpace(f.field))
	}
	if strings.TrimSpace(f.order) != "" {
		q.Set("order", strings.TrimSpace(f.order))
	}
	enc := q.Encode()
	if enc == "" {
		return ""
	}
	return "?" + enc
}

func runLBServiceList(args []string) int {
	fs := flag.NewFlagSet("lbsvc-list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var cf commonFlags
	cf.add(fs)

	var lf listFlags
	lf.add(fs)

	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if strings.TrimSpace(cf.path) == "" {
		cf.path = "/api/v2.1/load-balancers/domain/" + url.PathEscape(cf.orgName) + "/project/" + url.PathEscape(cf.projectID) + "/load-balancers/lb_service/" + lf.toQuery()
	}
	if err := cf.validate(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}

	code, err := do(cf, http.MethodGet, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if code < 200 || code >= 300 {
		return 1
	}
	return 0
}

func runLBServiceVIPList(args []string) int {
	fs := flag.NewFlagSet("lbsvc-vip", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var cf commonFlags
	cf.add(fs)

	lbsvcID := ""
	fs.StringVar(&lbsvcID, "lbsvc-id", "", "LBService id/uuid")

	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if strings.TrimSpace(lbsvcID) == "" {
		fmt.Fprintln(os.Stderr, "--lbsvc-id is required")
		return 2
	}
	if strings.TrimSpace(cf.path) == "" {
		cf.path = "/api/v2.1/load-balancers/domain/" + url.PathEscape(cf.orgName) + "/project/" + url.PathEscape(cf.projectID) + "/load-balancers/lb_service/" + strings.TrimSpace(lbsvcID) + "/vip"
	}
	if err := cf.validate(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}

	code, err := do(cf, http.MethodGet, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if code < 200 || code >= 300 {
		return 1
	}
	return 0
}

func runVirtualServerList(args []string) int {
	fs := flag.NewFlagSet("vs-list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var cf commonFlags
	cf.add(fs)

	lbID := ""
	fs.StringVar(&lbID, "lb-id", "", "Load balancer id/uuid for virtual-servers path")

	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if strings.TrimSpace(lbID) == "" {
		fmt.Fprintln(os.Stderr, "--lb-id is required")
		return 2
	}
	if strings.TrimSpace(cf.path) == "" {
		cf.path = "/api/v2.1/load-balancers/domain/" + url.PathEscape(cf.orgName) + "/project/" + url.PathEscape(cf.projectID) + "/load-balancers/" + strings.TrimSpace(lbID) + "/virtual-servers"
	}
	if err := cf.validate(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}

	code, err := do(cf, http.MethodGet, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if code < 200 || code >= 300 {
		return 1
	}
	return 0
}

func runVirtualServerCreate(args []string) int {
	fs := flag.NewFlagSet("vs-create", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var cf commonFlags
	cf.add(fs)

	lbID := ""
	fs.StringVar(&lbID, "lb-id", "", "Load balancer id/uuid for virtual-servers path")

	var params multiFlag
	fs.Var(&params, "param", "Query param (repeatable), format: key=value")

	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if strings.TrimSpace(lbID) == "" {
		fmt.Fprintln(os.Stderr, "--lb-id is required")
		return 2
	}
	if len(params) == 0 {
		fmt.Fprintln(os.Stderr, "provide at least one --param key=value")
		return 2
	}
	if strings.TrimSpace(cf.path) == "" {
		cf.path = "/api/v2.1/load-balancers/domain/" + url.PathEscape(cf.orgName) + "/project/" + url.PathEscape(cf.projectID) + "/load-balancers/" + strings.TrimSpace(lbID) + "/virtual-servers"
	}
	if err := cf.validate(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}

	q := url.Values{}
	for _, kv := range params {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) != 2 {
			fmt.Fprintf(os.Stderr, "invalid --param %q (want key=value)\n", kv)
			return 2
		}
		k := strings.TrimSpace(parts[0])
		v := strings.TrimSpace(parts[1])
		if k == "" {
			fmt.Fprintf(os.Stderr, "invalid --param %q (empty key)\n", kv)
			return 2
		}
		q.Add(k, v)
	}

	if enc := q.Encode(); enc != "" {
		if strings.Contains(cf.path, "?") {
			cf.path += "&" + enc
		} else {
			cf.path += "?" + enc
		}
	}
	code, err := do(cf, http.MethodPost, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if code < 200 || code >= 300 {
		return 1
	}
	return 0
}

func runLBServiceVIPCreate(args []string) int {
	fs := flag.NewFlagSet("vip-create", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var cf commonFlags
	cf.add(fs)

	lbsvcID := ""
	vip := ""
	port := ""
	fs.StringVar(&lbsvcID, "lbsvc-id", "", "LBService id/uuid")
	fs.StringVar(&vip, "vip", "", "(deprecated) VIP address (Swagger does not define this parameter)")
	fs.StringVar(&port, "port", "", "(deprecated) VIP port (Swagger does not define this parameter)")

	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if strings.TrimSpace(lbsvcID) == "" {
		fmt.Fprintln(os.Stderr, "--lbsvc-id is required")
		return 2
	}
	if strings.TrimSpace(cf.path) == "" {
		cf.path = "/api/v2.1/load-balancers/domain/" + url.PathEscape(cf.orgName) + "/project/" + url.PathEscape(cf.projectID) + "/load-balancers/lb_service/" + strings.TrimSpace(lbsvcID) + "/vip"
	}
	if err := cf.validate(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}

	code, err := do(cf, http.MethodPost, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if code < 200 || code >= 300 {
		return 1
	}
	return 0
}

type commonFlags struct {
	endpoint  string
	path      string
	insecure  bool
	timeout   time.Duration
	ceAuth    string
	orgName   string
	projectID string
	username  string
	password  string
	headers   multiFlag
}

func (f *commonFlags) add(fs *flag.FlagSet) {
	fs.StringVar(&f.endpoint, "endpoint", "", "Base URL for CMP/CCP API, e.g. https://100.65.242.181")
	fs.StringVar(&f.path, "path", "", "Request path, e.g. /ccp/lbaas/v1/health")
	fs.BoolVar(&f.insecure, "insecure-skip-tls-verify", false, "Skip TLS verification (debug only)")
	fs.DurationVar(&f.timeout, "timeout", 15*time.Second, "HTTP timeout")

	fs.StringVar(&f.ceAuth, "ce-auth", "", "Ce-Auth token")
	fs.StringVar(&f.orgName, "organisation-name", "", "organisation-name/partition/tenant")
	fs.StringVar(&f.projectID, "project-id", "", "project-id")

	fs.StringVar(&f.username, "username", "", "Basic auth username")
	fs.StringVar(&f.password, "password", "", "Basic auth password")

	fs.Var(&f.headers, "header", "Extra header (repeatable), format: Key: Value")
}

func (f *commonFlags) validate() error {
	if strings.TrimSpace(f.endpoint) == "" {
		return fmt.Errorf("--endpoint is required")
	}
	if strings.TrimSpace(f.path) == "" {
		return fmt.Errorf("--path is required")
	}

	ceAuthMode := f.ceAuth != ""
	basicMode := f.username != "" || f.password != ""

	modeCount := 0
	if ceAuthMode {
		modeCount++
	}
	if basicMode {
		modeCount++
	}
	if modeCount > 1 {
		return fmt.Errorf("choose only one auth mode: Ce-Auth or basic auth")
	}

	if ceAuthMode {
		missing := []string{}
		if f.ceAuth == "" {
			missing = append(missing, "--ce-auth")
		}
		if f.orgName == "" {
			missing = append(missing, "--organisation-name")
		}
		if f.projectID == "" {
			missing = append(missing, "--project-id")
		}
		if len(missing) > 0 {
			return fmt.Errorf("Ce-Auth auth selected but missing: %s", strings.Join(missing, ", "))
		}
		return nil
	}

	if basicMode {
		missing := []string{}
		if f.username == "" {
			missing = append(missing, "--username")
		}
		if f.password == "" {
			missing = append(missing, "--password")
		}
		if len(missing) > 0 {
			return fmt.Errorf("basic auth selected but missing: %s", strings.Join(missing, ", "))
		}
		return nil
	}

	if f.orgName != "" || f.projectID != "" {
		return fmt.Errorf("--organisation-name/--project-id provided without an auth token (use --ce-auth)")
	}
	return fmt.Errorf("provide auth via Ce-Auth (--ce-auth/--organisation-name/--project-id) or basic auth (--username/--password)")
}

func (f *commonFlags) httpClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{}
	}
	transport.TLSClientConfig.InsecureSkipVerify = f.insecure //nolint:gosec // Explicit debug flag.

	return &http.Client{
		Timeout:   f.timeout,
		Transport: transport,
	}
}

func (f *commonFlags) applyAuthHeaders(req *http.Request) {
	if f.ceAuth != "" {
		req.Header.Set("Ce-Auth", f.ceAuth)
		req.Header.Set("organisation-name", f.orgName)
		req.Header.Set("organization-name", f.orgName)
		req.Header.Set("project-id", f.projectID)
		return
	}
	if f.username != "" && f.password != "" {
		req.SetBasicAuth(f.username, f.password)
	}
}

func runLBServiceCreate(args []string) int {
	fs := flag.NewFlagSet("lbsvc-create", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var cf commonFlags
	cf.add(fs)

	name := ""
	desc := ""
	flavorID := ""
	vpcID := ""
	networkID := ""
	vpcName := ""
	fs.StringVar(&name, "name", "", "LBService name (must be unique)")
	fs.StringVar(&desc, "description", "", "Description")
	fs.StringVar(&flavorID, "flavor-id", "", "Flavor ID")
	fs.StringVar(&vpcID, "vpc-id", "", "VPC UUID")
	fs.StringVar(&networkID, "network-id", "", "Network UUID")
	fs.StringVar(&vpcName, "vpc-name", "", "VPC name")

	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if strings.TrimSpace(cf.path) == "" {
		cf.path = "/api/v2.1/load-balancers/domain/" + url.PathEscape(cf.orgName) + "/project/" + url.PathEscape(cf.projectID) + "/load-balancers/lb_service/"
	}
	if err := cf.validate(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}

	missing := []string{}
	if name == "" {
		missing = append(missing, "--name")
	}
	if flavorID == "" {
		missing = append(missing, "--flavor-id")
	}
	if vpcID == "" {
		missing = append(missing, "--vpc-id")
	}
	if networkID == "" {
		missing = append(missing, "--network-id")
	}
	if vpcName == "" {
		missing = append(missing, "--vpc-name")
	}
	if len(missing) > 0 {
		fmt.Fprintf(os.Stderr, "missing required flags: %s\n", strings.Join(missing, ", "))
		return 2
	}

	form := url.Values{}
	form.Set("name", name)
	form.Set("flavor_id", flavorID)
	form.Set("description", desc)
	form.Set("vpc_id", vpcID)
	form.Set("network_id", networkID)
	form.Set("vpc_name", vpcName)

	// Ensure form content-type; do() defaults to JSON but extra headers override.
	cf.headers = append(cf.headers, "Content-Type: application/x-www-form-urlencoded")

	code, err := do(cf, http.MethodPost, strings.NewReader(form.Encode()))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if code < 200 || code >= 300 {
		return 1
	}
	return 0
}

func runLBServiceGet(args []string) int {
	fs := flag.NewFlagSet("lbsvc-get", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var cf commonFlags
	cf.add(fs)

	id := ""
	fs.StringVar(&id, "id", "", "LBService id")

	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if strings.TrimSpace(id) == "" {
		fmt.Fprintln(os.Stderr, "--id is required")
		return 2
	}
	if strings.TrimSpace(cf.path) == "" {
		cf.path = "/api/v2.1/load-balancers/domain/" + url.PathEscape(cf.orgName) + "/project/" + url.PathEscape(cf.projectID) + "/load-balancers/lb_service/" + strings.TrimSpace(id) + "/"
	}
	if err := cf.validate(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}

	code, err := do(cf, http.MethodGet, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if code < 200 || code >= 300 {
		return 1
	}
	return 0
}

func (f *commonFlags) applyExtraHeaders(req *http.Request) error {
	for _, h := range f.headers {
		parts := strings.SplitN(h, ":", 2)
		if len(parts) != 2 {
			return fmt.Errorf("invalid --header %q (want 'Key: Value')", h)
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		if key == "" {
			return fmt.Errorf("invalid --header %q (empty key)", h)
		}
		req.Header.Set(key, val)
	}
	return nil
}

func runHealth(args []string) int {
	fs := flag.NewFlagSet("health", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var cf commonFlags
	cf.add(fs)

	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if err := cf.validate(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}

	code, err := do(cf, http.MethodGet, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if code < 200 || code >= 300 {
		return 1
	}
	return 0
}

func runRequest(args []string) int {
	fs := flag.NewFlagSet("request", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var cf commonFlags
	cf.add(fs)

	method := "GET"
	body := ""
	fs.StringVar(&method, "method", "GET", "HTTP method")
	fs.StringVar(&body, "body", "", "Request body. Use '-' to read from stdin")

	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if err := cf.validate(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}

	var bodyReader io.Reader
	if body != "" {
		if body == "-" {
			b, err := io.ReadAll(os.Stdin)
			if err != nil {
				fmt.Fprintf(os.Stderr, "reading stdin: %v\n", err)
				return 1
			}
			bodyReader = bytes.NewReader(b)
		} else {
			bodyReader = strings.NewReader(body)
		}
	}

	code, err := do(cf, strings.ToUpper(strings.TrimSpace(method)), bodyReader)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if code < 200 || code >= 300 {
		return 1
	}
	return 0
}

func do(cf commonFlags, method string, body io.Reader) (int, error) {
	endpoint := strings.TrimRight(cf.endpoint, "/")
	path := "/" + strings.TrimLeft(cf.path, "/")
	url := endpoint + path

	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return 0, fmt.Errorf("creating request: %w", err)
	}

	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	cf.applyAuthHeaders(req)
	if err := cf.applyExtraHeaders(req); err != nil {
		return 0, err
	}

	resp, err := cf.httpClient().Do(req)
	if err != nil {
		return 0, fmt.Errorf("%s %s failed: %w", method, url, err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	fmt.Fprintf(os.Stdout, "http_status=%d %s\n", resp.StatusCode, resp.Status)
	if len(respBody) > 0 {
		_, _ = os.Stdout.Write(respBody)
		if !bytes.HasSuffix(respBody, []byte("\n")) {
			_, _ = os.Stdout.Write([]byte("\n"))
		}
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("non-2xx status")
	}
	return resp.StatusCode, nil
}
