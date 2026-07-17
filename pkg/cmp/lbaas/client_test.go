package lbaas

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestListLoadBalancers_UsesCeAuthToken(t *testing.T) {
	var gotPath string
	var gotCeAuth string
	var gotOrg string
	var gotProject string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.String()
		gotCeAuth = r.Header.Get("Ce-Auth")
		gotOrg = r.Header.Get("organisation-name")
		gotProject = r.Header.Get("project-id")

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	c, err := New(srv.URL, Options{
		OrganisationName: "qa-tenant",
		ProjectID:        "3",
		CeAuthToken:      "id.exp.sig",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = c.ListLoadBalancers(context.Background(), ListOptions{Field: "id", Order: "desc"})
	if err != nil {
		t.Fatalf("ListLoadBalancers: %v", err)
	}

	if gotPath != "/api/v2.1/load-balancers/domain/qa-tenant/project/3/load-balancers/?field=id&order=desc" {
		t.Fatalf("unexpected path: %q", gotPath)
	}
	if gotCeAuth != "id.exp.sig" {
		t.Fatalf("expected Ce-Auth token, got %q", gotCeAuth)
	}
	if gotOrg != "qa-tenant" || gotProject != "3" {
		t.Fatalf("expected org/project headers, got org=%q project=%q", gotOrg, gotProject)
	}
}

func TestListLBServices_UsesGeneratedCeAuthAndTenancyHeaders(t *testing.T) {
	var gotPath string
	var gotCeAuth string
	var gotOrg string
	var gotProject string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.String()
		gotCeAuth = r.Header.Get("Ce-Auth")
		gotOrg = r.Header.Get("organisation-name")
		gotProject = r.Header.Get("project-id")

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	c, err := New(srv.URL, Options{
		OrganisationName: "qa-tenant",
		ProjectID:        "3",
		CeAuthAPIID:      "api-id",
		CeAuthSecret:     "secret",
		CeAuthExpiry:     10 * time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Freeze time so the token is deterministic.
	c.now = func() time.Time { return time.Unix(1000, 0) }

	_, err = c.ListLBServices(context.Background(), ListOptions{Offset: 0, Limit: 10, Search: "", Field: "created", Order: "desc"})
	if err != nil {
		t.Fatalf("ListLBServices: %v", err)
	}

	if !strings.HasPrefix(gotPath, "/api/v2.1/load-balancers/domain/qa-tenant/project/3/load-balancers/lb_service/") {
		t.Fatalf("unexpected path: %q", gotPath)
	}
	if !strings.Contains(gotPath, "offset=0") || !strings.Contains(gotPath, "limit=10") || !strings.Contains(gotPath, "search=") {
		t.Fatalf("expected offset/limit/search in query, got %q", gotPath)
	}

	expiryTs := int64(1010) // 1000 + 10s
	msg := fmt.Sprintf("%s.%d", "api-id", expiryTs)
	h := hmac.New(sha256.New, []byte("secret"))
	_, _ = h.Write([]byte(msg))
	expected := fmt.Sprintf("%s.%d.%s", "api-id", expiryTs, hex.EncodeToString(h.Sum(nil)))
	if gotCeAuth != expected {
		t.Fatalf("expected Ce-Auth %q, got %q", expected, gotCeAuth)
	}
	if gotOrg != "qa-tenant" || gotProject != "3" {
		t.Fatalf("expected org/project headers, got org=%q project=%q", gotOrg, gotProject)
	}
}

func TestCreateVirtualServer_UsesQueryParams(t *testing.T) {
	var gotPath string
	var gotCT string
	var gotBody string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.String()
		b, _ := io.ReadAll(r.Body)
		gotCT = r.Header.Get("Content-Type")
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c, err := New(srv.URL, Options{CeAuthToken: "tok"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	query := url.Values{}
	query.Set("name", "vs1")
	query.Set("description", "d")

	_, err = c.CreateVirtualServer(context.Background(), "lb123", query)
	if err != nil {
		t.Fatalf("CreateVirtualServer: %v", err)
	}
	if gotCT != "" {
		t.Fatalf("expected empty content-type, got %q", gotCT)
	}
	if gotBody != "" {
		t.Fatalf("expected empty body, got %q", gotBody)
	}
	if gotPath != "/api/v2.1/load-balancers/lb123/virtual-servers?description=d&name=vs1" {
		t.Fatalf("unexpected path: %q", gotPath)
	}
}

func TestCreateLBServiceVIP_DoesNotUseQueryParams(t *testing.T) {
	var gotPath string
	var gotCT string
	var gotBody string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.String()
		b, _ := io.ReadAll(r.Body)
		gotCT = r.Header.Get("Content-Type")
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c, err := New(srv.URL, Options{CeAuthToken: "tok"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	query := url.Values{}
	query.Set("vip", "10.0.0.10")
	query.Set("port", "6443")

	_, err = c.CreateLBServiceVIP(context.Background(), "lbsvc-1", query)
	if err != nil {
		t.Fatalf("CreateLBServiceVIP: %v", err)
	}
	if gotCT != "" {
		t.Fatalf("expected empty content-type, got %q", gotCT)
	}
	if gotBody != "" {
		t.Fatalf("expected empty body, got %q", gotBody)
	}
	if gotPath != "/api/v2.1/load-balancers/lb_service/lbsvc-1/vip" {
		t.Fatalf("unexpected path: %q", gotPath)
	}
}

func TestClient_DoesNotFollowRedirects(t *testing.T) {
	// If the client follows redirects, Go's default redirect behavior could
	// attempt to reach an internal hostname (e.g. neutron-service) and fail with
	// a transport error. We want to surface the 30x response instead.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "http://neutron-service/api/v1/load-balancers/")
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()

	c, err := New(srv.URL, Options{CeAuthToken: "tok"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = c.ListLoadBalancers(context.Background(), ListOptions{})
	if err == nil {
		t.Fatalf("expected error")
	}
	if !strings.Contains(err.Error(), "status 302") {
		t.Fatalf("expected 302 status error, got %v", err)
	}
}
