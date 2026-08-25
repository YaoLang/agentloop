package tools

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/YaoLang/agentloop/internal/memory"
)

func httpTestCatalog(base string, endpoints []HTTPEndpoint) *HTTPCatalog {
	u, err := url.Parse(base)
	if err != nil {
		panic(err)
	}
	return &HTTPCatalog{
		BaseURL:    base,
		AllowHosts: []string{u.Hostname()},
		Endpoints:  endpoints,
	}
}

func httpReg(t *testing.T, cat *HTTPCatalog) *Registry {
	t.Helper()
	ws := t.TempDir()
	mem, err := memory.Open(ws)
	if err != nil {
		t.Fatal(err)
	}
	return Default(Options{
		Workspace:   ws,
		Memory:      mem,
		ToolTimeout: 3 * time.Second,
		HTTP:        cat,
	})
}

func TestHTTPCallGETPathParam(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"123","ok":true}`)
	}))
	t.Cleanup(srv.Close)

	cat := httpTestCatalog(srv.URL, []HTTPEndpoint{
		{ID: "get_order", Method: "GET", Path: "/v1/orders/{id}", Description: "Get one order"},
	})
	reg := httpReg(t, cat)
	obs, err := reg.Call(context.Background(), "http_call", `{"endpoint":"get_order","path":{"id":"123"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/orders/123" {
		t.Fatalf("path=%q", gotPath)
	}
	if !strings.Contains(obs, "HTTP 200") {
		t.Fatalf("status line missing: %q", obs)
	}
	if !strings.Contains(obs, `"id":"123"`) && !strings.Contains(obs, `"ok":true`) {
		t.Fatalf("body missing: %q", obs)
	}
}

func TestHTTPCallPOSTJSONBody(t *testing.T) {
	var gotBody string
	var gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(srv.Close)

	cat := httpTestCatalog(srv.URL, []HTTPEndpoint{
		{ID: "create_order", Method: "POST", Path: "/v1/orders", Description: "Create order"},
	})
	reg := httpReg(t, cat)
	obs, err := reg.Call(context.Background(), "http_call", `{"endpoint":"create_order","body":{"sku":"x"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotCT, "application/json") {
		t.Fatalf("content-type=%q", gotCT)
	}
	if !strings.Contains(gotBody, `"sku"`) || !strings.Contains(gotBody, `"x"`) {
		t.Fatalf("body=%q", gotBody)
	}
	if !strings.Contains(obs, "HTTP 201") {
		t.Fatalf("obs=%q", obs)
	}
}

func TestHTTPCallUnknownEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("server should not be hit")
	}))
	t.Cleanup(srv.Close)
	cat := httpTestCatalog(srv.URL, []HTTPEndpoint{
		{ID: "list_orders", Method: "GET", Path: "/v1/orders"},
	})
	reg := httpReg(t, cat)
	_, err := reg.Call(context.Background(), "http_call", `{"endpoint":"nope"}`)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "unknown") {
		t.Fatalf("err=%v", err)
	}
}

func TestHTTPCallHostNotAllowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("server should not be hit")
	}))
	t.Cleanup(srv.Close)
	cat := &HTTPCatalog{
		BaseURL:    srv.URL,
		AllowHosts: []string{"example.com"},
		Endpoints:  []HTTPEndpoint{{ID: "x", Method: "GET", Path: "/"}},
	}
	reg := httpReg(t, cat)
	_, err := reg.Call(context.Background(), "http_call", `{"endpoint":"x"}`)
	if err == nil {
		t.Fatal("expected host error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "host") &&
		!strings.Contains(strings.ToLower(err.Error()), "allow") &&
		!strings.Contains(strings.ToLower(err.Error()), "blocked") {
		t.Fatalf("err=%v", err)
	}
}

func TestHTTPCallMetadataIPRefused(t *testing.T) {
	cat := &HTTPCatalog{
		BaseURL:    "http://169.254.169.254/",
		AllowHosts: []string{"example.com"},
		Endpoints:  []HTTPEndpoint{{ID: "meta", Method: "GET", Path: "/"}},
	}
	reg := httpReg(t, cat)
	_, err := reg.Call(context.Background(), "http_call", `{"endpoint":"meta"}`)
	if err == nil {
		t.Fatal("expected refuse")
	}
}

func TestHTTPCallSecretInjectedNotInObservation(t *testing.T) {
	const secret = "gho_SECRETVALUE_test"
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"login":"octocat"}`)
	}))
	t.Cleanup(srv.Close)

	u, _ := url.Parse(srv.URL)
	cat := &HTTPCatalog{
		BaseURL:    srv.URL,
		AllowHosts: []string{u.Hostname()},
		Auth: HTTPAuth{
			Secret: "shop_token",
			Header: "Authorization",
			Prefix: "Bearer ",
		},
		Endpoints: []HTTPEndpoint{{ID: "me", Method: "GET", Path: "/user"}},
	}
	reg := httpReg(t, cat)
	ctx := WithRuntime(context.Background(), Runtime{
		TenantID: "acme",
		Secret: func(name string) (string, bool) {
			if name == "shop_token" {
				return secret, true
			}
			return "", false
		},
	})
	obs, err := reg.Call(ctx, "http_call", `{"endpoint":"me"}`)
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer "+secret {
		t.Fatalf("auth on wire=%q", gotAuth)
	}
	if strings.Contains(obs, secret) {
		t.Fatalf("secret leaked in observation: %q", obs)
	}
	if err != nil && strings.Contains(err.Error(), secret) {
		t.Fatal("secret leaked in error")
	}
}

func TestHTTPCallRedactsJSONTokenKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"token":"abc","ok":true}`)
	}))
	t.Cleanup(srv.Close)
	cat := httpTestCatalog(srv.URL, []HTTPEndpoint{
		{ID: "tok", Method: "GET", Path: "/"},
	})
	reg := httpReg(t, cat)
	obs, err := reg.Call(context.Background(), "http_call", `{"endpoint":"tok"}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(obs, "abc") {
		t.Fatalf("token value visible: %q", obs)
	}
	if !strings.Contains(obs, "[redacted]") {
		t.Fatalf("expected [redacted]: %q", obs)
	}
	if !strings.Contains(obs, "ok") {
		t.Fatalf("ok key lost: %q", obs)
	}
}

func TestHTTPCallIgnoresModelURLAndMethod(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(srv.Close)
	cat := httpTestCatalog(srv.URL, []HTTPEndpoint{
		{ID: "get_order", Method: "GET", Path: "/v1/orders/{id}"},
	})
	reg := httpReg(t, cat)
	args := `{"endpoint":"get_order","path":{"id":"123"},"url":"http://evil.example/steal","method":"DELETE","headers":{"Authorization":"Bearer evil"}}`
	obs, err := reg.Call(context.Background(), "http_call", args)
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodGet {
		t.Fatalf("method=%q (model method must be ignored)", gotMethod)
	}
	if gotPath != "/v1/orders/123" {
		t.Fatalf("path=%q", gotPath)
	}
	if !strings.Contains(obs, "HTTP 200") {
		t.Fatalf("obs=%q", obs)
	}
}

func TestHTTPCallCrossHostRedirectRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://example.org/elsewhere", http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	cat := httpTestCatalog(srv.URL, []HTTPEndpoint{
		{ID: "go", Method: "GET", Path: "/"},
	})
	reg := httpReg(t, cat)
	_, err := reg.Call(context.Background(), "http_call", `{"endpoint":"go"}`)
	if err == nil {
		t.Fatal("expected redirect error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "redirect") &&
		!strings.Contains(strings.ToLower(err.Error()), "host") {
		t.Fatalf("err=%v", err)
	}
}

func TestDefaultHTTPCallRegistration(t *testing.T) {
	ws := t.TempDir()
	mem, err := memory.Open(ws)
	if err != nil {
		t.Fatal(err)
	}
	reg := Default(Options{Workspace: ws, Memory: mem})
	if _, ok := reg.Get("http_call"); ok {
		t.Fatal("http_call registered without catalog")
	}
	cat := &HTTPCatalog{
		BaseURL:   "https://api.example.com",
		Endpoints: []HTTPEndpoint{{ID: "list_orders", Method: "GET", Path: "/v1/orders", Description: "List orders"}},
	}
	reg = Default(Options{Workspace: ws, Memory: mem, HTTP: cat})
	tool, ok := reg.Get("http_call")
	if !ok {
		t.Fatal("http_call not registered with catalog")
	}
	if !strings.Contains(tool.Description, "list_orders") {
		t.Fatalf("description missing endpoint id: %s", tool.Description)
	}
	if !strings.Contains(tool.Description, "GET") || !strings.Contains(tool.Description, "/v1/orders") {
		t.Fatalf("description missing method/path: %s", tool.Description)
	}
	if !strings.Contains(tool.Description, "List orders") {
		t.Fatalf("description missing text: %s", tool.Description)
	}
}

func TestLoadHTTPCatalogTempDirAndInvalid(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "http.json")
	raw := []byte(`{
		"base_url": "https://api.example.com",
		"allow_hosts": ["api.example.com"],
		"endpoints": [{"id":"list_orders","method":"GET","path":"/v1/orders","description":"List orders"}]
	}`)
	if err := os.WriteFile(good, raw, 0o640); err != nil {
		t.Fatal(err)
	}
	cat, err := LoadHTTPCatalog(good)
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Endpoints) != 1 || cat.Endpoints[0].ID != "list_orders" {
		t.Fatalf("%+v", cat)
	}

	missing := filepath.Join(dir, "missing.json")
	if _, err := LoadHTTPCatalog(missing); err == nil || !os.IsNotExist(err) {
		t.Fatalf("missing: %v", err)
	}

	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte(`{not json`), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadHTTPCatalog(bad); err == nil {
		t.Fatal("expected invalid json")
	}

	empty := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(empty, []byte(`{"base_url":"https://api.example.com","endpoints":[]}`), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadHTTPCatalog(empty); err == nil {
		t.Fatal("expected empty endpoints")
	}

	unsafe := filepath.Join(dir, "unsafe.json")
	if err := os.WriteFile(unsafe, []byte(`{"base_url":"ftp://api.example.com","endpoints":[{"id":"x","method":"GET","path":"/"}]}`), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadHTTPCatalog(unsafe); err == nil {
		t.Fatal("expected unsafe base_url")
	}

	userinfo := filepath.Join(dir, "userinfo.json")
	if err := os.WriteFile(userinfo, []byte(`{"base_url":"https://u:p@api.example.com","endpoints":[{"id":"x","method":"GET","path":"/"}]}`), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadHTTPCatalog(userinfo); err == nil {
		t.Fatal("expected userinfo refuse")
	}

	badMethod := filepath.Join(dir, "method.json")
	if err := os.WriteFile(badMethod, []byte(`{"base_url":"https://api.example.com","endpoints":[{"id":"x","method":"TRACE","path":"/"}]}`), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadHTTPCatalog(badMethod); err == nil {
		t.Fatal("expected unsupported method")
	}
}

func TestHTTPCallQueryAndPathEscape(t *testing.T) {
	var got url.URL
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = *r.URL
		_, _ = io.WriteString(w, `ok`)
	}))
	t.Cleanup(srv.Close)
	cat := httpTestCatalog(srv.URL, []HTTPEndpoint{
		{ID: "q", Method: "GET", Path: "/v1/find/{q}"},
	})
	reg := httpReg(t, cat)
	_, err := reg.Call(context.Background(), "http_call", `{"endpoint":"q","path":{"q":"a/b"},"query":{"limit":10,"tag":"x"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Path, "a") || strings.Contains(got.Path, "a/b") {
		// PathEscape should encode the slash in the value
		if got.Path != "/v1/find/a%2Fb" && got.EscapedPath() != "/v1/find/a%2Fb" {
			t.Fatalf("path=%q escaped=%q", got.Path, got.EscapedPath())
		}
	}
	if got.Query().Get("limit") != "10" || got.Query().Get("tag") != "x" {
		t.Fatalf("query=%q", got.RawQuery)
	}
}

func TestHTTPCallHTTPErrorIsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"err":"missing"}`)
	}))
	t.Cleanup(srv.Close)
	cat := httpTestCatalog(srv.URL, []HTTPEndpoint{
		{ID: "x", Method: "GET", Path: "/"},
	})
	reg := httpReg(t, cat)
	obs, err := reg.Call(context.Background(), "http_call", `{"endpoint":"x"}`)
	if err != nil {
		t.Fatalf("4xx must be err=nil: %v", err)
	}
	if !strings.Contains(obs, "HTTP 404") {
		t.Fatalf("obs=%q", obs)
	}
}

func TestHTTPCallMissingSecret(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("should not send without secret")
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	cat := &HTTPCatalog{
		BaseURL:    srv.URL,
		AllowHosts: []string{u.Hostname()},
		Auth:       HTTPAuth{Secret: "shop_token", Prefix: "Bearer "},
		Endpoints:  []HTTPEndpoint{{ID: "x", Method: "GET", Path: "/"}},
	}
	reg := httpReg(t, cat)
	_, err := reg.Call(context.Background(), "http_call", `{"endpoint":"x"}`)
	if err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("err=%v", err)
	}
	if strings.Contains(err.Error(), "gho_") {
		t.Fatal("leaked value")
	}
}

func TestHTTPCallDefaultAllowHosts(t *testing.T) {
	raw := []byte(`{"base_url":"https://api.example.com/v1","endpoints":[{"id":"x","method":"get","path":"/ok"}]}`)
	dir := t.TempDir()
	p := filepath.Join(dir, "http.json")
	if err := os.WriteFile(p, raw, 0o640); err != nil {
		t.Fatal(err)
	}
	cat, err := LoadHTTPCatalog(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.AllowHosts) != 1 || !strings.EqualFold(cat.AllowHosts[0], "api.example.com") {
		t.Fatalf("allow_hosts=%v", cat.AllowHosts)
	}
	if cat.Endpoints[0].Method != http.MethodGet {
		t.Fatalf("method=%q", cat.Endpoints[0].Method)
	}
}

func TestRedactJSONKeys(t *testing.T) {
	in := []byte(`{"token":"abc","nested":{"api_key":"k","password":"p"},"ok":true}`)
	out, ok := redactJSON(in)
	if !ok {
		t.Fatal("redact failed")
	}
	if strings.Contains(out, "abc") || strings.Contains(out, `"k"`) || strings.Contains(out, `"p"`) {
		t.Fatalf("values visible: %s", out)
	}
	if !strings.Contains(out, "[redacted]") || !strings.Contains(out, "ok") {
		t.Fatalf("out=%s", out)
	}
}

func TestHTTPCallSchemaRequiresEndpoint(t *testing.T) {
	cat := &HTTPCatalog{
		BaseURL:   "https://api.example.com",
		Endpoints: []HTTPEndpoint{{ID: "x", Method: "GET", Path: "/"}},
	}
	reg := httpReg(t, cat)
	err := reg.Validate("http_call", `{}`)
	if err == nil || !strings.Contains(err.Error(), "endpoint") {
		t.Fatalf("err=%v", err)
	}
}
