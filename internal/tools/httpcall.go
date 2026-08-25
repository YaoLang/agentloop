package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	httpClientTimeout = 10 * time.Second
	httpBodyLimit     = 64 << 10
	maxRedirects      = 5
)

var (
	redactKeyRe = regexp.MustCompile(`(?i)token|secret|password|authorization|api[_-]?key`)
	pathParamRe = regexp.MustCompile(`\{([^{}]+)\}`)
)

var allowedHTTPMethods = map[string]bool{
	http.MethodGet:    true,
	http.MethodPost:   true,
	http.MethodPut:    true,
	http.MethodPatch:  true,
	http.MethodDelete: true,
}

// HTTPAuth names a tenant secret injected as a request header.
// Secret is a name (Runtime.Secret), never a literal token.
type HTTPAuth struct {
	Secret string `json:"secret"`
	Header string `json:"header"`
	Prefix string `json:"prefix"`
}

// HTTPEndpoint is one catalog entry. The model picks id only.
type HTTPEndpoint struct {
	ID          string `json:"id"`
	Method      string `json:"method"`
	Path        string `json:"path"`
	Description string `json:"description"`
}

// HTTPCatalog is data/tenants/{id}/http.json (outside workspace/).
type HTTPCatalog struct {
	BaseURL    string         `json:"base_url"`
	AllowHosts []string       `json:"allow_hosts"`
	Auth       HTTPAuth       `json:"auth"`
	Endpoints  []HTTPEndpoint `json:"endpoints"`
}

type httpCallArgs struct {
	Endpoint string          `json:"endpoint"`
	Path     map[string]any  `json:"path"`
	Query    map[string]any  `json:"query"`
	Body     json.RawMessage `json:"body"`
}

// LoadHTTPCatalog reads and validates a catalog file.
// Missing file returns the os.IsNotExist error from ReadFile.
func LoadHTTPCatalog(path string) (*HTTPCatalog, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cat HTTPCatalog
	if err := json.Unmarshal(raw, &cat); err != nil {
		return nil, fmt.Errorf("http catalog: invalid json")
	}
	if err := cat.normalize(); err != nil {
		return nil, err
	}
	return &cat, nil
}

func (c *HTTPCatalog) normalize() error {
	if c == nil {
		return fmt.Errorf("http catalog: empty")
	}
	u, err := url.Parse(strings.TrimSpace(c.BaseURL))
	if err != nil || u == nil || u.Host == "" {
		return fmt.Errorf("http catalog: unsafe base_url")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("http catalog: unsafe base_url")
	}
	if u.User != nil {
		return fmt.Errorf("http catalog: unsafe base_url")
	}
	if len(c.AllowHosts) == 0 {
		c.AllowHosts = []string{u.Hostname()}
	}
	if len(c.Endpoints) == 0 {
		return fmt.Errorf("http catalog: empty endpoints")
	}
	for i := range c.Endpoints {
		ep := &c.Endpoints[i]
		ep.ID = strings.TrimSpace(ep.ID)
		if ep.ID == "" {
			return fmt.Errorf("http catalog: empty endpoint id")
		}
		ep.Method = strings.ToUpper(strings.TrimSpace(ep.Method))
		if !allowedHTTPMethods[ep.Method] {
			return fmt.Errorf("http catalog: unsupported method %q", ep.Method)
		}
	}
	return nil
}

func (c *HTTPCatalog) lookup(id string) (HTTPEndpoint, bool) {
	if c == nil {
		return HTTPEndpoint{}, false
	}
	for _, ep := range c.Endpoints {
		if ep.ID == id {
			return ep, true
		}
	}
	return HTTPEndpoint{}, false
}

func cloneCatalog(c *HTTPCatalog) *HTTPCatalog {
	if c == nil {
		return &HTTPCatalog{}
	}
	cp := *c
	cp.AllowHosts = append([]string(nil), c.AllowHosts...)
	cp.Endpoints = append([]HTTPEndpoint(nil), c.Endpoints...)
	return &cp
}

func httpCallDescription(cat *HTTPCatalog) string {
	var b strings.Builder
	b.WriteString("Call a tenant-catalog HTTP endpoint. You cannot set URL, method, or Authorization; pick an endpoint id.")
	b.WriteString("\nAvailable endpoints:")
	if cat == nil {
		return b.String()
	}
	for _, ep := range cat.Endpoints {
		fmt.Fprintf(&b, "\n- %s %s %s", ep.ID, ep.Method, ep.Path)
		if ep.Description != "" {
			fmt.Fprintf(&b, " — %s", ep.Description)
		}
	}
	return b.String()
}

// HTTPCallTool is the single outbound HTTP tool. URL, method, and
// Authorization come from the catalog, never from model arguments.
func HTTPCallTool(opt Options) *Tool {
	cat := cloneCatalog(opt.HTTP)
	_ = cat.normalize()
	return &Tool{
		Name:        "http_call",
		Description: httpCallDescription(cat),
		Timeout:     opt.ToolTimeout,
		Schema: map[string]any{
			"type":     "object",
			"required": required("endpoint"),
			"properties": map[string]any{
				"endpoint": prop("string"),
				"path":     prop("object"),
				"query":    prop("object"),
				"body":     prop("object"),
			},
		},
		Handler: func(ctx context.Context, argsJSON string) (string, error) {
			return handleHTTPCall(ctx, cat, argsJSON)
		},
	}
}

func handleHTTPCall(ctx context.Context, cat *HTTPCatalog, argsJSON string) (string, error) {
	var a httpCallArgs
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return "", err
	}
	ep, ok := cat.lookup(a.Endpoint)
	if !ok {
		return "", fmt.Errorf("http_call: unknown endpoint")
	}
	u, err := buildRequestURL(cat.BaseURL, ep.Path, a.Path, a.Query)
	if err != nil {
		return "", err
	}
	if err := checkSSRF(u, cat.AllowHosts); err != nil {
		return "", err
	}

	var bodyReader io.Reader
	if methodAllowsBody(ep.Method) && len(bytes.TrimSpace(a.Body)) > 0 && string(bytes.TrimSpace(a.Body)) != "null" {
		bodyReader = bytes.NewReader(a.Body)
	}

	req, err := http.NewRequestWithContext(ctx, ep.Method, u.String(), bodyReader)
	if err != nil {
		return "", fmt.Errorf("http_call: %w", err)
	}
	if bodyReader != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	var secretVal string
	if name := strings.TrimSpace(cat.Auth.Secret); name != "" {
		rt, ok := RuntimeFrom(ctx)
		if !ok {
			return "", fmt.Errorf("http_call: secret %q not configured", name)
		}
		val, ok := rt.Secret(name)
		if !ok || val == "" {
			return "", fmt.Errorf("http_call: secret %q not configured", name)
		}
		secretVal = val
		header := cat.Auth.Header
		if header == "" {
			header = "Authorization"
		}
		req.Header.Set(header, cat.Auth.Prefix+val)
	}

	client := &http.Client{
		Timeout: httpClientTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("http_call: too many redirects")
			}
			if err := checkRedirectURL(req.URL, via[0].URL.Hostname()); err != nil {
				return err
			}
			return nil
		},
	}
	res, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("http_call: %w", redactErr(err, secretVal))
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, httpBodyLimit))
	if err != nil {
		return "", fmt.Errorf("http_call: read body: %w", redactErr(err, secretVal))
	}
	body := redactObservation(raw, secretVal)
	return fmt.Sprintf("HTTP %d\n%s", res.StatusCode, body), nil
}

func methodAllowsBody(m string) bool {
	switch m {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
		return true
	default:
		return false
	}
}

func buildRequestURL(baseURL, pathTmpl string, pathArgs, queryArgs map[string]any) (*url.URL, error) {
	base, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || base == nil || base.Host == "" {
		return nil, fmt.Errorf("http_call: unsafe base_url")
	}
	if base.Scheme != "http" && base.Scheme != "https" {
		return nil, fmt.Errorf("http_call: scheme must be http or https")
	}
	if base.User != nil {
		return nil, fmt.Errorf("http_call: userinfo not allowed")
	}
	filled, err := fillPath(pathTmpl, pathArgs)
	if err != nil {
		return nil, err
	}
	if filled != "" && !strings.HasPrefix(filled, "/") {
		filled = "/" + filled
	}
	u := *base
	u.User = nil
	u.Fragment = ""
	u.RawPath = ""
	u.Path = strings.TrimRight(base.Path, "/") + filled
	q := u.Query()
	for k, v := range queryArgs {
		q.Set(k, stringify(v))
	}
	u.RawQuery = q.Encode()
	return &u, nil
}

func fillPath(tmpl string, params map[string]any) (string, error) {
	if tmpl == "" {
		return "", nil
	}
	var miss string
	out := pathParamRe.ReplaceAllStringFunc(tmpl, func(m string) string {
		name := m[1 : len(m)-1]
		if params == nil {
			miss = name
			return m
		}
		v, ok := params[name]
		if !ok {
			miss = name
			return m
		}
		return url.PathEscape(stringify(v))
	})
	if miss != "" {
		return "", fmt.Errorf("http_call: missing path param %q", miss)
	}
	if strings.Contains(out, "{") {
		return "", fmt.Errorf("http_call: unfilled path placeholder")
	}
	return out, nil
}

func stringify(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case json.Number:
		return x.String()
	case float64:
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	case float32:
		f := float64(x)
		if f == float64(int64(f)) {
			return strconv.FormatInt(int64(f), 10)
		}
		return strconv.FormatFloat(f, 'f', -1, 32)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case bool:
		return strconv.FormatBool(x)
	default:
		return fmt.Sprint(v)
	}
}

func checkSSRF(u *url.URL, allow []string) error {
	if u == nil {
		return fmt.Errorf("http_call: empty url")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("http_call: scheme must be http or https")
	}
	if u.User != nil {
		return fmt.Errorf("http_call: userinfo not allowed")
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("http_call: empty host")
	}
	if !hostAllowed(host, allow) {
		return fmt.Errorf("http_call: host not allowed")
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("http_call: resolve: %w", err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("http_call: resolve: no addresses")
	}
	literal := net.ParseIP(host)
	for _, ip := range ips {
		if !isBlockedIP(ip) {
			continue
		}
		if literal != nil && ip.Equal(literal) {
			continue
		}
		return fmt.Errorf("http_call: blocked address")
	}
	return nil
}

func checkRedirectURL(u *url.URL, origHost string) error {
	if u == nil {
		return fmt.Errorf("http_call: empty redirect")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("http_call: scheme must be http or https")
	}
	if u.User != nil {
		return fmt.Errorf("http_call: userinfo not allowed")
	}
	if !strings.EqualFold(u.Hostname(), origHost) {
		return fmt.Errorf("http_call: cross-host redirect refused")
	}
	return nil
}

func hostAllowed(host string, allow []string) bool {
	h := strings.ToLower(host)
	for _, a := range allow {
		if strings.EqualFold(strings.TrimSpace(a), h) {
			return true
		}
	}
	return false
}

func isBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}
	if ip.Equal(net.ParseIP("169.254.169.254")) {
		return true
	}
	return false
}

func redactObservation(raw []byte, secret string) string {
	s := string(raw)
	if redacted, ok := redactJSON(raw); ok {
		s = redacted
	}
	if secret != "" {
		s = strings.ReplaceAll(s, secret, "[redacted]")
	}
	return s
}

func redactJSON(raw []byte) (string, bool) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", false
	}
	redactWalk(v)
	out, err := json.Marshal(v)
	if err != nil {
		return "", false
	}
	return string(out), true
}

func redactWalk(v any) {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if redactKeyRe.MatchString(k) {
				x[k] = "[redacted]"
				continue
			}
			redactWalk(val)
		}
	case []any:
		for _, val := range x {
			redactWalk(val)
		}
	}
}

func redactErr(err error, secret string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if secret != "" && strings.Contains(msg, secret) {
		return fmt.Errorf("%s", strings.ReplaceAll(msg, secret, "[redacted]"))
	}
	return err
}
