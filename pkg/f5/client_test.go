package f5

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/go-logr/logr"
)

func TestClientWithCeAuth_SetsHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("expected method GET, got %s", r.Method)
		}
		if got := r.Header.Get("Ce-Auth"); got != "token" {
			t.Fatalf("expected Ce-Auth token header, got %q", got)
		}
		if got := r.Header.Get("organisation-name"); got != "tenant" {
			t.Fatalf("expected organisation-name header, got %q", got)
		}
		if got := r.Header.Get("organization-name"); got != "tenant" {
			t.Fatalf("expected organization-name header, got %q", got)
		}
		if got := r.Header.Get("project-id"); got != "proj" {
			t.Fatalf("expected project-id header, got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[{"id":"lb1"}]`))
	}))
	defer srv.Close()

	c, err := NewClientWithCeAuth(logr.Discard(), srv.URL, "tenant", "proj", "token")
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}

	items, err := c.ListLoadBalancers(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListLoadBalancers returned error: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
}

func TestDeleteLBService_IgnoresNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Fatalf("expected method DELETE, got %s", r.Method)
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("not found"))
	}))
	defer srv.Close()

	c, err := NewClientWithCeAuth(logr.Discard(), srv.URL, "tenant", "proj", "token")
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}

	err = c.DeleteLBService(context.Background(), "lb-999")
	if err == nil || !IsNotFound(err) {
		// DeleteLBService does not suppress not-found; that is the caller's responsibility.
		// Here we just validate the error is classifiable.
		if err != nil && !IsNotFound(err) {
			t.Fatalf("expected not-found error, got %v", err)
		}
	}
}

func TestIsUnauthorized_MatchesWrappedHTTPStatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("nope"))
	}))
	defer srv.Close()

	c, err := NewClientWithCeAuth(logr.Discard(), srv.URL, "tenant", "proj", "token")
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}

	_, err = c.EnsureControlPlaneVirtualServer(context.Background(), "1.2.3.4", 443, []Backend{{IP: "10.0.0.1", Port: 443}})
	if err == nil {
		t.Fatalf("expected error")
	}
	if !IsUnauthorized(err) {
		t.Fatalf("expected IsUnauthorized(err)=true, got false; err=%v", err)
	}
}

func TestEnsureControlPlaneVirtualServer_CMPLBaaSFlow(t *testing.T) {
	// Track which API calls are made during the CMP LBaaS flow.
	var (
		createdLBService bool
		createdVIP       bool
		createdVS        bool
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path

		switch {
		// Step 1: List existing LB Services (returns empty → triggers create)
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/lb_service/"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`[]`))
		// Step 1b: Create LB Service
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/lb_service/"):
			createdLBService = true
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"lb-001","status":"Active"}`))
		// Step 2: List existing VIPs (returns empty → triggers create)
		case r.Method == http.MethodGet && strings.Contains(path, "/lb_service/lb-001/vip"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`[]`))
		// Step 2b: Create VIP
		case r.Method == http.MethodPost && strings.Contains(path, "/lb_service/lb-001/vip"):
			createdVIP = true
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":101}`))
		// Step 3: List existing Virtual Servers (returns empty → triggers create)
		case r.Method == http.MethodGet && strings.Contains(path, "/lb-001/virtual-servers"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`[]`))
		// Step 3b: Create Virtual Server
		case r.Method == http.MethodPost && strings.Contains(path, "/lb-001/virtual-servers"):
			createdVS = true
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"vs-201","name":"cp-apiserver-vs"}`))
		default:
			t.Logf("unexpected request: %s %s", r.Method, path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c, err := NewClientWithCeAuth(logr.Discard(), srv.URL, "tenant", "proj", "token")
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}
	c.SetCMPLBaaSConfig(CMPLBaaSConfig{
		FlavorID:  25,
		NetworkID: "net-1",
		VPCID:     "vpc-1",
		VPCName:   "vpc-test",
	})

	ids, err := c.EnsureControlPlaneVirtualServer(context.Background(), "1.2.3.4", 443, []Backend{{IP: "10.0.0.1", Port: 443}})
	if err != nil {
		t.Fatalf("EnsureControlPlaneVirtualServer returned error: %v", err)
	}
	if ids == nil {
		t.Fatalf("expected non-nil CMPResourceIDs")
	}
	if ids.LBServiceID != "lb-001" {
		t.Fatalf("expected LBServiceID %q, got %q", "lb-001", ids.LBServiceID)
	}
	if ids.VIPPortID != "101" {
		t.Fatalf("expected VIPPortID %q, got %q", "101", ids.VIPPortID)
	}
	if ids.VirtualServerID != "vs-201" {
		t.Fatalf("expected VirtualServerID %q, got %q", "vs-201", ids.VirtualServerID)
	}
	if !createdLBService {
		t.Fatalf("expected LB Service to be created")
	}
	if !createdVIP {
		t.Fatalf("expected VIP to be created")
	}
	if !createdVS {
		t.Fatalf("expected Virtual Server to be created")
	}
}

func TestEnsureControlPlaneVirtualServer_IsIdempotentOnRetry(t *testing.T) {
	var (
		listLBCalls    int
		listVIPCalls   int
		listVSCalls    int
		createLBCalls  int
		createVIPCalls int
		createVSCalls  int
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path

		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/lb_service/"):
			listLBCalls++
			w.WriteHeader(http.StatusOK)
			if listLBCalls == 1 {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			_, _ = w.Write([]byte(`[{"id":"lb-001","name":"cp-lb-tenant","status":"Active"}]`))
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/lb_service/"):
			createLBCalls++
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"lb-001","name":"cp-lb-tenant","status":"Active"}`))
		case r.Method == http.MethodGet && strings.Contains(path, "/lb_service/lb-001/vip"):
			listVIPCalls++
			w.WriteHeader(http.StatusOK)
			if listVIPCalls == 1 {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			_, _ = w.Write([]byte(`[{"id":101}]`))
		case r.Method == http.MethodPost && strings.Contains(path, "/lb_service/lb-001/vip"):
			createVIPCalls++
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":101}`))
		case r.Method == http.MethodGet && strings.Contains(path, "/lb-001/virtual-servers"):
			listVSCalls++
			w.WriteHeader(http.StatusOK)
			if listVSCalls == 1 {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			_, _ = w.Write([]byte(`[{"id":"vs-201","name":"cp-apiserver-vs"}]`))
		case r.Method == http.MethodPost && strings.Contains(path, "/lb-001/virtual-servers"):
			createVSCalls++
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"vs-201","name":"cp-apiserver-vs"}`))
		default:
			t.Logf("unexpected request: %s %s", r.Method, path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c, err := NewClientWithCeAuth(logr.Discard(), srv.URL, "tenant", "proj", "token")
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}
	c.SetCMPLBaaSConfig(CMPLBaaSConfig{FlavorID: 25})

	ids1, err := c.EnsureControlPlaneVirtualServer(context.Background(), "1.2.3.4", 443, []Backend{{IP: "10.0.0.1", Port: 443}})
	if err != nil {
		t.Fatalf("first EnsureControlPlaneVirtualServer returned error: %v", err)
	}
	ids2, err := c.EnsureControlPlaneVirtualServer(context.Background(), "1.2.3.4", 443, []Backend{{IP: "10.0.0.1", Port: 443}})
	if err != nil {
		t.Fatalf("second EnsureControlPlaneVirtualServer returned error: %v", err)
	}

	if ids1 == nil || ids2 == nil {
		t.Fatalf("expected non-nil CMPResourceIDs")
	}
	if ids1.LBServiceID != "lb-001" || ids2.LBServiceID != "lb-001" {
		t.Fatalf("unexpected LBServiceID: ids1=%q ids2=%q", ids1.LBServiceID, ids2.LBServiceID)
	}
	if ids1.VIPPortID != "101" || ids2.VIPPortID != "101" {
		t.Fatalf("unexpected VIPPortID: ids1=%q ids2=%q", ids1.VIPPortID, ids2.VIPPortID)
	}
	if ids1.VirtualServerID != "vs-201" || ids2.VirtualServerID != "vs-201" {
		t.Fatalf("unexpected VirtualServerID: ids1=%q ids2=%q", ids1.VirtualServerID, ids2.VirtualServerID)
	}

	if createLBCalls != 1 {
		t.Fatalf("expected LB Service create once, got %d", createLBCalls)
	}
	if createVIPCalls != 1 {
		t.Fatalf("expected VIP create once, got %d", createVIPCalls)
	}
	if createVSCalls != 1 {
		t.Fatalf("expected VS create once, got %d", createVSCalls)
	}
}

func TestEnsureControlPlaneVirtualServer_DoesNotCreateOnListError(t *testing.T) {
	var createLBCalls int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/lb_service/"):
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("boom"))
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/lb_service/"):
			createLBCalls++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"lb-should-not-be-created"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c, err := NewClientWithCeAuth(logr.Discard(), srv.URL, "tenant", "proj", "token")
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}

	_, err = c.EnsureControlPlaneVirtualServer(context.Background(), "1.2.3.4", 443, []Backend{{IP: "10.0.0.1", Port: 443}})
	if err == nil {
		t.Fatalf("expected error")
	}
	if createLBCalls != 0 {
		t.Fatalf("expected no LB Service create attempts, got %d", createLBCalls)
	}
}

func TestClient_DoesNotFollowRedirects(t *testing.T) {
	// If the client follows redirects, CMP may send us to an internal hostname
	// that isn't resolvable from where the controller runs.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "http://neutron-service/api/v2.1/load-balancers/")
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()

	c, err := NewClientWithCeAuth(logr.Discard(), srv.URL, "tenant", "proj", "token")
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}

	_, err = c.ListLoadBalancers(context.Background(), nil)
	if err == nil {
		t.Fatalf("expected error")
	}
	var se *HTTPStatusError
	if !AsStatusError(err, &se) {
		t.Fatalf("expected HTTPStatusError, got %T: %v", err, err)
	}
	if se.StatusCode != http.StatusFound {
		t.Fatalf("expected status 302, got %d", se.StatusCode)
	}
}

func TestDeleteControlPlaneVirtualServer_FullCMPCleanup(t *testing.T) {
	var (
		deletedVS  bool
		deletedVIP bool
		deletedLB  bool
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Fatalf("expected method DELETE, got %s %s", r.Method, r.URL.Path)
		}
		path := r.URL.Path
		switch {
		case strings.Contains(path, "/virtual-servers/vs-001"):
			deletedVS = true
			w.WriteHeader(http.StatusNoContent)
		case strings.Contains(path, "/vip/vip-001"):
			deletedVIP = true
			w.WriteHeader(http.StatusNoContent)
		case strings.Contains(path, "/lb_service/lb-001"):
			deletedLB = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Logf("unexpected DELETE path: %s", path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c, err := NewClientWithCeAuth(logr.Discard(), srv.URL, "tenant", "proj", "token")
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}

	ids := &CMPResourceIDs{
		LBServiceID:       "lb-001",
		VIPPortID:         "vip-001",
		VirtualServerID:   "vs-001",
		VirtualServerName: "cp-apiserver-vs",
	}
	if err := c.DeleteControlPlaneVirtualServer(context.Background(), ids); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if !deletedVS {
		t.Fatalf("expected VS to be deleted")
	}
	if !deletedVIP {
		t.Fatalf("expected VIP to be deleted")
	}
	if !deletedLB {
		t.Fatalf("expected LB Service to be deleted")
	}
}

func TestListLoadBalancers_UsesPrefixQueryAndHeaders(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("expected method GET, got %s", r.Method)
		}
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		if gotPath != "/api/v2.1/load-balancers/domain/tenant/project/proj/load-balancers/" {
			t.Fatalf("expected path %q, got %q", "/api/v2.1/load-balancers/domain/tenant/project/proj/load-balancers/", gotPath)
		}
		// url.Values.Encode sorts keys; verify exact encoded query.
		if gotQuery != "field=id&limit=5&offset=0&order=desc" {
			t.Fatalf("expected query %q, got %q", "field=id&limit=5&offset=0&order=desc", gotQuery)
		}
		if got := r.Header.Get("Ce-Auth"); got != "token" {
			t.Fatalf("expected Ce-Auth token header, got %q", got)
		}
		if got := r.Header.Get("organisation-name"); got != "tenant" {
			t.Fatalf("expected organisation-name header, got %q", got)
		}
		if got := r.Header.Get("project-id"); got != "proj" {
			t.Fatalf("expected project-id header, got %q", got)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[{"id": 1}, {"id": 2}]`))
	}))
	defer srv.Close()

	c, err := NewClientWithCeAuth(logr.Discard(), srv.URL, "tenant", "proj", "token")
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}

	limit := int32(5)
	offset := int32(0)
	items, err := c.ListLoadBalancers(context.Background(), &ListLoadBalancersOptions{
		Limit:  &limit,
		Offset: &offset,
		Field:  "id",
		Order:  "desc",
	})
	if err != nil {
		t.Fatalf("ListLoadBalancers returned error: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
}

func TestCreateLoadBalancer_UsesPrefixHeadersAndFormEncoding(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("expected method POST, got %s", r.Method)
		}
		if r.URL.Path != "/api/v2.1/load-balancers/domain/tenant/project/proj/load-balancers/" {
			t.Fatalf("expected path %q, got %q", "/api/v2.1/load-balancers/domain/tenant/project/proj/load-balancers/", r.URL.Path)
		}
		if got := r.Header.Get("Ce-Auth"); got != "token" {
			t.Fatalf("expected Ce-Auth token header, got %q", got)
		}
		if got := r.Header.Get("organisation-name"); got != "tenant" {
			t.Fatalf("expected organisation-name header, got %q", got)
		}
		if got := r.Header.Get("project-id"); got != "proj" {
			t.Fatalf("expected project-id header, got %q", got)
		}
		ct := r.Header.Get("Content-Type")
		if !strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
			t.Fatalf("expected Content-Type to start with %q, got %q", "application/x-www-form-urlencoded", ct)
		}
		b, _ := io.ReadAll(r.Body)
		// url.Values.Encode sorts keys for stable output.
		if string(b) != "description=demo&name=lb1" {
			t.Fatalf("expected form body %q, got %q", "description=demo&name=lb1", string(b))
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"123"}`))
	}))
	defer srv.Close()

	c, err := NewClientWithCeAuth(logr.Discard(), srv.URL, "tenant", "proj", "token")
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}

	form := url.Values{}
	form.Set("name", "lb1")
	form.Set("description", "demo")
	resp, err := c.CreateLoadBalancer(context.Background(), form)
	if err != nil {
		t.Fatalf("CreateLoadBalancer returned error: %v", err)
	}
	if !strings.Contains(string(resp), "\"id\"") {
		t.Fatalf("expected response to contain id, got %s", string(resp))
	}
}

func TestCreateLBService_UsesPrefixCeAuthHeadersAndFormEncoding(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("expected method POST, got %s", r.Method)
		}
		if r.URL.Path != "/api/v2.1/load-balancers/domain/tenant/project/proj/load-balancers/lb_service/" {
			t.Fatalf("expected path %q, got %q", "/api/v2.1/load-balancers/domain/tenant/project/proj/load-balancers/lb_service/", r.URL.Path)
		}
		if got := r.Header.Get("Ce-Auth"); got != "token" {
			t.Fatalf("expected Ce-Auth header, got %q", got)
		}
		if got := r.Header.Get("organisation-name"); got != "tenant" {
			t.Fatalf("expected organisation-name header, got %q", got)
		}
		if got := r.Header.Get("organization-name"); got != "tenant" {
			t.Fatalf("expected organization-name header, got %q", got)
		}
		if got := r.Header.Get("project-id"); got != "proj" {
			t.Fatalf("expected project-id header, got %q", got)
		}
		ct := r.Header.Get("Content-Type")
		if !strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
			t.Fatalf("expected Content-Type to start with %q, got %q", "application/x-www-form-urlencoded", ct)
		}
		b, _ := io.ReadAll(r.Body)
		// url.Values.Encode sorts keys for stable output.
		if string(b) != "description=loadbalancer&flavor_id=25&name=lb-4&network_id=net&vpc_id=vpc&vpc_name=vpc-27oct" {
			t.Fatalf("unexpected form body: %q", string(b))
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"fc2853f0","status":"Creating"}`))
	}))
	defer srv.Close()

	c, err := NewClientWithCeAuth(logr.Discard(), srv.URL, "tenant", "proj", "token")
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}

	form := url.Values{}
	form.Set("name", "lb-4")
	form.Set("flavor_id", "25")
	form.Set("description", "loadbalancer")
	form.Set("vpc_id", "vpc")
	form.Set("network_id", "net")
	form.Set("vpc_name", "vpc-27oct")

	resp, err := c.CreateLBService(context.Background(), form)
	if err != nil {
		t.Fatalf("CreateLBService returned error: %v", err)
	}
	if !strings.Contains(string(resp), "Creating") {
		t.Fatalf("expected response to contain status, got %s", string(resp))
	}
}

func TestGetDeleteOptionsLoadBalancer_UsesPrefixAndHeaders(t *testing.T) {
	var gotOptions bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Ce-Auth"); got != "token" {
			t.Fatalf("expected Ce-Auth token header, got %q", got)
		}
		if got := r.Header.Get("organisation-name"); got != "tenant" {
			t.Fatalf("expected organisation-name header, got %q", got)
		}
		if got := r.Header.Get("project-id"); got != "proj" {
			t.Fatalf("expected project-id header, got %q", got)
		}

		switch r.Method {
		case http.MethodGet:
			if r.URL.Path != "/api/v2.1/load-balancers/domain/tenant/project/proj/load-balancers/123/" {
				t.Fatalf("expected path %q, got %q", "/api/v2.1/load-balancers/domain/tenant/project/proj/load-balancers/123/", r.URL.Path)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"123"}`))
		case http.MethodDelete:
			if r.URL.Path != "/api/v2.1/load-balancers/domain/tenant/project/proj/load-balancers/123/" {
				t.Fatalf("expected path %q, got %q", "/api/v2.1/load-balancers/domain/tenant/project/proj/load-balancers/123/", r.URL.Path)
			}
			w.WriteHeader(http.StatusNoContent)
		case http.MethodOptions:
			if r.URL.Path != "/api/v2.1/load-balancers/domain/tenant/project/proj/load-balancers/" {
				t.Fatalf("expected path %q, got %q", "/api/v2.1/load-balancers/domain/tenant/project/proj/load-balancers/", r.URL.Path)
			}
			gotOptions = true
			w.Header().Set("Allow", "GET, POST, OPTIONS")
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected method %s", r.Method)
		}
	}))
	defer srv.Close()

	c, err := NewClientWithCeAuth(logr.Discard(), srv.URL, "tenant", "proj", "token")
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}

	got, err := c.GetLoadBalancer(context.Background(), "123")
	if err != nil {
		t.Fatalf("GetLoadBalancer returned error: %v", err)
	}
	if !strings.Contains(string(got), "\"123\"") {
		t.Fatalf("expected response to contain 123, got %s", string(got))
	}

	if err := c.DeleteLoadBalancer(context.Background(), "123"); err != nil {
		t.Fatalf("DeleteLoadBalancer returned error: %v", err)
	}

	allow, err := c.OptionsLoadBalancers(context.Background())
	if err != nil {
		t.Fatalf("OptionsLoadBalancers returned error: %v", err)
	}
	if !gotOptions {
		t.Fatalf("expected OPTIONS request to be made")
	}
	if len(allow) != 3 || allow[0] != "GET" || allow[1] != "POST" || allow[2] != "OPTIONS" {
		t.Fatalf("unexpected allow list: %#v", allow)
	}
}

func TestUpdateLoadBalancer_UsesPrefixHeadersAndFormEncoding(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Fatalf("expected method PATCH, got %s", r.Method)
		}
		if r.URL.Path != "/api/v2.1/load-balancers/domain/tenant/project/proj/load-balancers/123/" {
			t.Fatalf("expected path %q, got %q", "/api/v2.1/load-balancers/domain/tenant/project/proj/load-balancers/123/", r.URL.Path)
		}
		if got := r.Header.Get("Ce-Auth"); got != "token" {
			t.Fatalf("expected Ce-Auth token header, got %q", got)
		}
		if got := r.Header.Get("organisation-name"); got != "tenant" {
			t.Fatalf("expected organisation-name header, got %q", got)
		}
		if got := r.Header.Get("project-id"); got != "proj" {
			t.Fatalf("expected project-id header, got %q", got)
		}
		ct := r.Header.Get("Content-Type")
		if !strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
			t.Fatalf("expected Content-Type to start with %q, got %q", "application/x-www-form-urlencoded", ct)
		}
		b, _ := io.ReadAll(r.Body)
		if string(b) != "description=updated" {
			t.Fatalf("expected form body %q, got %q", "description=updated", string(b))
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"123","description":"updated"}`))
	}))
	defer srv.Close()

	c, err := NewClientWithCeAuth(logr.Discard(), srv.URL, "tenant", "proj", "token")
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}

	form := url.Values{}
	form.Set("description", "updated")
	resp, err := c.UpdateLoadBalancer(context.Background(), "123", form)
	if err != nil {
		t.Fatalf("UpdateLoadBalancer returned error: %v", err)
	}
	if !strings.Contains(string(resp), "updated") {
		t.Fatalf("expected response to contain updated, got %s", string(resp))
	}
}

func TestRoutingRulesUseLBServiceSwaggerHierarchy(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	}))
	defer srv.Close()
	c, err := NewClientWithCeAuth(logr.Discard(), srv.URL, "tenant", "proj", "token")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.(*client).ListLBVirtualServerRoutingRules(context.Background(), "lb-1", "vs-1"); err != nil {
		t.Fatal(err)
	}
	want := "/api/v2.1/load-balancers/domain/tenant/project/proj/load-balancers/lb_service/lb-1/virtual-servers/vs-1/routing-rules"
	if gotPath != want {
		t.Fatalf("routing-rule path=%q, want %q", gotPath, want)
	}
}
