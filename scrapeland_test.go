package scrapeland

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestClient(t *testing.T, srv *httptest.Server, opts ...Option) *Client {
	t.Helper()
	c, err := New("pb_live_TESTKEY", append([]Option{WithAPIBase(srv.URL)}, opts...)...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.Backoff = time.Millisecond // keep retry tests fast
	return c
}

func TestNewRequiresKey(t *testing.T) {
	t.Setenv("SCRAPELAND_API_KEY", "")
	if _, err := New(""); err != ErrNoAPIKey {
		t.Errorf("New(\"\") err = %v, want ErrNoAPIKey", err)
	}
}

func TestNewReadsEnv(t *testing.T) {
	t.Setenv("SCRAPELAND_API_KEY", "pb_live_FROMENV")
	c, err := New("")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.APIKey != "pb_live_FROMENV" {
		t.Errorf("APIKey = %q", c.APIKey)
	}
}

func TestProxyUsername(t *testing.T) {
	c, _ := New("KEY", WithCountry("US"), WithSession("job42"))
	tests := []struct {
		name string
		opts *Options
		want string
	}{
		{"client defaults", nil, "KEY-country-us-session-job42"},
		{"per-request overrides default", &Options{Country: "de"}, "KEY-country-de-session-job42"},
		{"all knobs", &Options{Country: "gb", Protocol: "socks5", Session: "s1", MaxLatency: 3000},
			"KEY-country-gb-protocol-socks5-session-s1-maxlatency-3000"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.ProxyUsername(tc.opts); got != tc.want {
				t.Errorf("ProxyUsername() = %q, want %q", got, tc.want)
			}
		})
	}
}

// The key must land in the username with an EMPTY password — url.User would drop
// the colon and the gateway would not see a credential pair at all.
func TestProxyURLCarriesKeyAsUsername(t *testing.T) {
	c, _ := New("pb_live_ABC", WithGateway("http://gw.example:8080"))
	u, err := c.ProxyURL(nil)
	if err != nil {
		t.Fatalf("ProxyURL: %v", err)
	}
	if u.Host != "gw.example:8080" {
		t.Errorf("host = %q", u.Host)
	}
	if got := u.User.Username(); got != "pb_live_ABC" {
		t.Errorf("username = %q", got)
	}
	if pw, set := u.User.Password(); !set || pw != "" {
		t.Errorf("password = %q set=%v, want empty-but-set", pw, set)
	}
}

// A broken gateway URL must fail loudly. Silently falling back to a direct
// (un-proxied) transport would leak the caller's real IP to the target.
func TestHTTPClientFailsClosedOnBadGateway(t *testing.T) {
	c, _ := New("KEY", WithGateway("http://[::1]:namedport"))
	_, err := c.HTTPClient(nil).Get("https://example.com")
	if err == nil {
		t.Fatal("expected an error from a client built on a bad gateway URL")
	}
	if !strings.Contains(err.Error(), "bad gateway URL") {
		t.Errorf("err = %v, want it to name the bad gateway URL", err)
	}
}

func TestFetchSendsKeyAndPayload(t *testing.T) {
	var gotKey, gotPath string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey, gotPath = r.Header.Get("X-Api-Key"), r.URL.Path
		json.NewDecoder(r.Body).Decode(&body)
		json.NewEncoder(w).Encode(Result{URL: "https://e.com", Status: 200, Markdown: "# hi"})
	}))
	defer srv.Close()

	c := newTestClient(t, srv, WithCountry("us"))
	res, err := c.Fetch(context.Background(), "https://e.com", &Options{Format: "markdown"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if gotKey != "pb_live_TESTKEY" {
		t.Errorf("X-Api-Key = %q", gotKey)
	}
	if gotPath != "/v1/fetch" {
		t.Errorf("path = %q", gotPath)
	}
	if body["format"] != "markdown" {
		t.Errorf("format = %v", body["format"])
	}
	// The client-level country must survive being given an Options for something else.
	if body["country"] != "us" {
		t.Errorf("country = %v, want the client default to be applied", body["country"])
	}
	if res.Markdown != "# hi" {
		t.Errorf("markdown = %q", res.Markdown)
	}
}

// Unset Options fields must be omitted entirely, not sent as zero values — a
// stray "render":false or "format":"" changes how the server treats the request.
func TestZeroOptionsAreOmitted(t *testing.T) {
	var raw map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&raw)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	if _, err := c.Fetch(context.Background(), "https://e.com", nil); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	for _, k := range []string{"format", "render", "country", "screenshot", "fields", "count"} {
		if _, present := raw[k]; present {
			t.Errorf("payload should omit %q when unset, got %v", k, raw[k])
		}
	}
	if raw["url"] != "https://e.com" {
		t.Errorf("url = %v", raw["url"])
	}
}

func TestRetriesTransientThenSucceeds(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		json.NewEncoder(w).Encode(Result{Status: 200, HTML: "<p>ok</p>"})
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	res, err := c.Fetch(context.Background(), "https://e.com", nil)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3 (two retries)", calls)
	}
	if res.HTML != "<p>ok</p>" {
		t.Errorf("html = %q", res.HTML)
	}
}

// A 4xx that is not 429 will not improve on retry — burning attempts on it just
// delays the error and bills the caller for nothing.
func TestDoesNotRetryClientErrors(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusPaymentRequired)
		w.Write([]byte(`{"error":"quota exhausted"}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	_, err := c.Fetch(context.Background(), "https://e.com", nil)
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (402 must not be retried)", calls)
	}
	var apiErr *APIError
	if !errorsAs(err, &apiErr) {
		t.Fatalf("err = %T (%v), want *APIError", err, err)
	}
	if apiErr.Status != http.StatusPaymentRequired {
		t.Errorf("status = %d", apiErr.Status)
	}
	if !strings.Contains(apiErr.Body, "quota exhausted") {
		t.Errorf("body = %q, want the server message preserved", apiErr.Body)
	}
}

func TestSearchAndBatchShapes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/search":
			w.Write([]byte(`{"query":"go","results":[{"title":"T","url":"https://u","snippet":"S"}]}`))
		case "/v1/batch":
			w.Write([]byte(`{"results":[{"url":"https://a","status":200},{"url":"https://b","status":404}]}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	sr, err := c.Search(context.Background(), "go", &Options{Count: 5})
	if err != nil || len(sr.Results) != 1 || sr.Results[0].Title != "T" {
		t.Fatalf("Search = %+v, err=%v", sr, err)
	}
	br, err := c.Batch(context.Background(), []string{"https://a", "https://b"}, nil)
	if err != nil || len(br.Results) != 2 || br.Results[1].Status != 404 {
		t.Fatalf("Batch = %+v, err=%v", br, err)
	}
}

func TestContextCancelStopsRetrying(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	c.Backoff = time.Second // long enough that cancellation wins the race
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.Fetch(ctx, "https://e.com", nil); err == nil {
		t.Fatal("expected an error once the context expires")
	}
}

// errors.As without importing errors twice in the test file.
func errorsAs(err error, target **APIError) bool {
	for err != nil {
		if e, ok := err.(*APIError); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
