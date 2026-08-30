// Package scrapeland is a client for the scrape.land web data extraction API.
//
// There are two ways in, and most Go programs want the first:
//
//   - Client.HTTPClient returns a *http.Client wired to the rotating gateway.
//     Every request it makes exits from a fresh IP. Nothing else in your code
//     has to change — hand it to any library that accepts an *http.Client.
//   - Client.Fetch / Extract / Batch / Search call the data API directly and
//     return parsed results, for when you want structured data rather than a
//     transport.
//
// Control parameters (country, sticky session, protocol, latency ceiling) are
// encoded into the proxy username, so they work through the plain transport too:
//
//	pb_live_KEY-country-us-session-job42-protocol-socks5-maxlatency-3000
//
// Zero dependencies: standard library only.
package scrapeland

import (
	"bytes"
	"context"
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
)

const (
	// DefaultGateway is the forward proxy. Port 8080 is the long-standing
	// endpoint; 443 also serves the proxy for networks that filter 8080.
	DefaultGateway = "http://gateway.scrape.land:8080"
	// DefaultAPIBase is the data API — a different host from the proxy.
	DefaultAPIBase = "https://scrape.land"
)

// retryStatuses are worth another attempt: a fresh request rotates to a new exit
// IP, which routinely turns a transient upstream failure into a success.
var retryStatuses = map[int]bool{429: true, 500: true, 502: true, 503: true, 504: true}

// BatchMaxURLs is the server's per-call cap for Batch; BatchIter chunks by it.
const BatchMaxURLs = 20

// ErrNoAPIKey is returned by New when no key was passed and none is in the
// environment.
var ErrNoAPIKey = errors.New("scrapeland: api key required (pass it or set $SCRAPELAND_API_KEY)")

// APIError is a non-2xx response from the data API. Status is the HTTP code;
// Body is the (truncated) response body, which usually carries a JSON message;
// RetryAfter is the server's Retry-After, when it sent one. Branch on the kind
// with the Is* helpers or check Status directly:
//
//	var apiErr *scrapeland.APIError
//	if errors.As(err, &apiErr) && apiErr.IsRateLimited() { time.Sleep(apiErr.RetryAfter) }
type APIError struct {
	Status     int
	Endpoint   string
	Body       string
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	return fmt.Sprintf("scrapeland: %s returned HTTP %d: %s", e.Endpoint, e.Status, e.Body)
}

// IsBadRequest reports a 400 — a malformed or contradictory request (a retry won't help).
func (e *APIError) IsBadRequest() bool { return e.Status == http.StatusBadRequest }

// IsUnauthorized reports a 401 — the API key is missing, invalid, or revoked.
func (e *APIError) IsUnauthorized() bool { return e.Status == http.StatusUnauthorized }

// IsPaymentRequired reports a 402 — out of quota / prepaid credit, or key budget spent.
func (e *APIError) IsPaymentRequired() bool { return e.Status == http.StatusPaymentRequired }

// IsNotFound reports a 404 — no such resource (e.g. an unknown job id).
func (e *APIError) IsNotFound() bool { return e.Status == http.StatusNotFound }

// IsRateLimited reports a 429 — too many requests; honor RetryAfter.
func (e *APIError) IsRateLimited() bool { return e.Status == http.StatusTooManyRequests }

// IsServer reports a 5xx — a transient server-side failure, usually worth retrying.
func (e *APIError) IsServer() bool { return e.Status >= 500 }

// Client talks to the scrape.land gateway and data API. The zero value is not
// usable — construct it with New.
type Client struct {
	APIKey  string
	Gateway string
	APIBase string

	// Defaults applied to every request; per-request Options override them.
	Country      string
	Session      string
	Protocol     string
	MaxLatencyMS int

	// Retries counts EXTRA attempts after the first (default 2). Backoff is the
	// base delay, doubled per attempt.
	Retries int
	Backoff time.Duration

	// HTTP is used for data API calls. It is NOT proxied — the data API is a
	// normal HTTPS endpoint. Use HTTPClient for proxied traffic.
	HTTP *http.Client
}

// Option configures a Client.
type Option func(*Client)

// WithGateway overrides the proxy endpoint.
func WithGateway(gw string) Option { return func(c *Client) { c.Gateway = gw } }

// WithAPIBase overrides the data API base URL.
func WithAPIBase(base string) Option { return func(c *Client) { c.APIBase = base } }

// WithCountry sets a default exit country (ISO code, e.g. "us").
func WithCountry(cc string) Option { return func(c *Client) { c.Country = cc } }

// WithSession sets a default sticky-session id (same exit IP across requests).
func WithSession(id string) Option { return func(c *Client) { c.Session = id } }

// WithProtocol restricts the upstream protocol ("http", "https", "socks5").
func WithProtocol(p string) Option { return func(c *Client) { c.Protocol = p } }

// WithMaxLatency only uses upstreams faster than the given milliseconds.
func WithMaxLatency(ms int) Option { return func(c *Client) { c.MaxLatencyMS = ms } }

// WithHTTPClient supplies the client used for data API calls (timeouts, tracing).
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.HTTP = h } }

// WithRetries sets how many EXTRA attempts follow a failure.
func WithRetries(n int) Option { return func(c *Client) { c.Retries = n } }

// New builds a Client. apiKey falls back to $SCRAPELAND_API_KEY. Gateway and API
// base fall back to $SCRAPELAND_GATEWAY / $SCRAPELAND_API_BASE, then the public
// endpoints.
func New(apiKey string, opts ...Option) (*Client, error) {
	c := &Client{
		APIKey:  firstNonEmpty(apiKey, os.Getenv("SCRAPELAND_API_KEY")),
		Gateway: firstNonEmpty(os.Getenv("SCRAPELAND_GATEWAY"), DefaultGateway),
		APIBase: firstNonEmpty(os.Getenv("SCRAPELAND_API_BASE"), DefaultAPIBase),
		Retries: 2,
		Backoff: 500 * time.Millisecond,
		// 180s, matching the API's own budget: an AI extraction on the "smart"
		// model runs 70-90s, so a 60s client timeout cut it off before the answer.
		HTTP: &http.Client{Timeout: 180 * time.Second},
	}
	for _, o := range opts {
		o(c)
	}
	if c.APIKey == "" {
		return nil, ErrNoAPIKey
	}
	c.APIBase = strings.TrimRight(c.APIBase, "/")
	c.Gateway = strings.TrimRight(c.Gateway, "/")
	return c, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// ---- proxy plumbing -------------------------------------------------------

// ProxyUsername builds the proxy username: the API key plus control parameters.
// Per-request values in o override the client defaults.
func (c *Client) ProxyUsername(o *Options) string {
	country, session, protocol, maxLat := c.Country, c.Session, c.Protocol, c.MaxLatencyMS
	if o != nil {
		country = firstNonEmpty(o.Country, country)
		session = firstNonEmpty(o.Session, session)
		protocol = firstNonEmpty(o.Protocol, protocol)
		if o.MaxLatency > 0 {
			maxLat = o.MaxLatency
		}
	}
	// Order matters only for readability — the gateway parses by name — but keep
	// it stable so sticky sessions hash consistently in customer logs.
	var b strings.Builder
	b.WriteString(c.APIKey)
	if country != "" {
		b.WriteString("-country-" + strings.ToLower(country))
	}
	if protocol != "" {
		b.WriteString("-protocol-" + protocol)
	}
	if session != "" {
		b.WriteString("-session-" + session)
	}
	if maxLat > 0 {
		b.WriteString("-maxlatency-" + strconv.Itoa(maxLat))
	}
	return b.String()
}

// ProxyURL returns the proxy URL for use with http.Transport.Proxy.
func (c *Client) ProxyURL(o *Options) (*url.URL, error) {
	host := c.Gateway
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	// The key goes in the username with an EMPTY password. url.User (not
	// url.UserPassword) would omit the colon, and the gateway reads the pair.
	return url.Parse("http://" + url.QueryEscape(c.ProxyUsername(o)) + ":@" + host)
}

// HTTPClient returns an *http.Client whose every request exits from a fresh IP.
// Pass nil for o to use the client defaults.
//
//	c, _ := scrapeland.New(key)
//	res, err := c.HTTPClient(nil).Get("https://api.ipify.org")
//
// Each call builds a fresh Transport, because the control parameters live in the
// proxy credentials — sharing one across different Options would silently apply
// the first caller's country and session to everyone else.
func (c *Client) HTTPClient(o *Options) *http.Client {
	proxy, err := c.ProxyURL(o)
	timeout := 180 * time.Second
	if c.HTTP != nil && c.HTTP.Timeout != 0 {
		timeout = c.HTTP.Timeout
	}
	if err != nil {
		// A malformed gateway URL is a construction-time mistake, not a per-request
		// one. Return a client that fails loudly on first use rather than silently
		// making DIRECT (un-proxied) requests, which would leak the caller's own IP.
		return &http.Client{
			Timeout: timeout,
			Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
				return nil, fmt.Errorf("scrapeland: bad gateway URL %q: %w", c.Gateway, err)
			}),
		}
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: &http.Transport{Proxy: http.ProxyURL(proxy)},
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// ---- data API -------------------------------------------------------------

// Options are the per-request knobs shared by Fetch, Extract, Batch and Search.
// Every field is optional; the zero value means "use the client default, or the
// server default". It doubles as the request payload, which is why the json tags
// matter as much as the field names.
type Options struct {
	Format         string            `json:"format,omitempty"` // "html" (default), "text", "markdown", "raw"
	Country        string            `json:"country,omitempty"`
	Session        string            `json:"session,omitempty"`
	Protocol       string            `json:"protocol,omitempty"`
	MaxLatency     int               `json:"max_latency,omitempty"`
	Render         bool              `json:"render,omitempty"`
	Device         string            `json:"device,omitempty"`
	BlockResources bool              `json:"block_resources,omitempty"`
	Fingerprint    bool              `json:"fingerprint,omitempty"`
	WaitFor        string            `json:"wait_for,omitempty"`
	Headers        bool              `json:"headers,omitempty"`
	Screenshot     bool              `json:"screenshot,omitempty"`
	FullPage       bool              `json:"full_page,omitempty"`
	SendHeaders    map[string]string `json:"send_headers,omitempty"`
	Cookies        string            `json:"cookies,omitempty"`
	Method         string            `json:"method,omitempty"`
	Body           string            `json:"body,omitempty"`
	Actions        []any             `json:"actions,omitempty"`
	Metadata       bool              `json:"metadata,omitempty"`
	Links          bool              `json:"links,omitempty"`

	// Extraction. Fields holds CSS/XPath selectors; Prompt drives AI extraction.
	Fields      map[string]any `json:"fields,omitempty"`
	Prompt      string         `json:"prompt,omitempty"`
	Schema      any            `json:"schema,omitempty"`
	Model       string         `json:"model,omitempty"`
	ExtractType string         `json:"extract_type,omitempty"`

	// TopK caps the list returned by Rank (rank only; ignored elsewhere).
	TopK int `json:"top_k,omitempty"`

	// Structured selects what a Prompt returns. Nil (the default) and true give a JSON
	// object in Result.Data; false gives a plain-language reply in Result.Answer and
	// leaves Data nil. A pointer so "unset" and "explicitly false" differ — use
	// scrapeland.Bool(false) to ask for prose.
	//
	// Schema and ExtractType both pin a JSON shape and so imply structured output;
	// combining either with Structured=Bool(false) is rejected with a 400.
	Structured *bool `json:"structured,omitempty"`

	// Search only.
	Count int `json:"count,omitempty"`
}

// payload embeds Options so every knob is marshalled inline, rather than copying
// twenty fields by hand on every call site the way the other clients must.
type payload struct {
	URL  string   `json:"url,omitempty"`
	URLs []string `json:"urls,omitempty"`
	Q    string   `json:"q,omitempty"`
	*Options
}

// applyDefaults fills unset routing knobs from the client so a caller who passes
// Options for one field doesn't silently lose the client-wide country/session.
func (c *Client) applyDefaults(o *Options) *Options {
	out := &Options{}
	if o != nil {
		*out = *o
	}
	out.Country = firstNonEmpty(out.Country, c.Country)
	out.Session = firstNonEmpty(out.Session, c.Session)
	out.Protocol = firstNonEmpty(out.Protocol, c.Protocol)
	if out.MaxLatency == 0 {
		out.MaxLatency = c.MaxLatencyMS
	}
	return out
}

// Result is one fetched page. Which body field is populated depends on Format.
type Result struct {
	URL         string            `json:"url"`
	Status      int               `json:"status"`
	HTML        string            `json:"html,omitempty"`
	Text        string            `json:"text,omitempty"`
	Markdown    string            `json:"markdown,omitempty"`
	Base64      string            `json:"base64,omitempty"`
	ContentType string            `json:"content_type,omitempty"`
	Screenshot  string            `json:"screenshot,omitempty"`
	Data        map[string]any    `json:"data,omitempty"`
	Answer      string            `json:"answer,omitempty"`
	Metadata    map[string]any    `json:"metadata,omitempty"`
	Links       []string          `json:"links,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Error       string            `json:"error,omitempty"`
}

// Bool returns a pointer to b, for the optional tri-state Options fields (today:
// Structured, where nil and false mean different things). Saves every caller writing
// the same two-line temporary.
func Bool(b bool) *bool { return &b }

// BatchResult is the response from Batch: one entry per requested URL, in order.
type BatchResult struct {
	Results []Result `json:"results"`
}

// RankedLink is one link scored by Rank: Score is 0-1, Reason is a short rationale.
type RankedLink struct {
	URL    string  `json:"url"`
	Text   string  `json:"text"`
	Score  float64 `json:"score"`
	Reason string  `json:"reason"`
}

// RankResult is the response from Rank. If the AI step failed, Links come back in
// document order (not ranked) and RankingError explains why — Rank returns no error
// in that case, so always check RankingError when the order matters.
type RankResult struct {
	URL          string       `json:"url"`
	Status       int          `json:"status"`
	Query        string       `json:"query"`
	Links        []RankedLink `json:"links"`
	RankingError string       `json:"ranking_error,omitempty"`
}

// rankPayload is Batch/Fetch's payload shape plus the rank-specific query. TopK and
// Model ride along inside the embedded Options.
type rankPayload struct {
	URL   string `json:"url"`
	Query string `json:"query"`
	*Options
}

// SearchResult is the response from Search.
type SearchResult struct {
	Query   string `json:"query"`
	Results []struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Snippet string `json:"snippet"`
	} `json:"results"`
}

// Job is an async job record.
type Job struct {
	ID     string          `json:"id"`
	Type   string          `json:"type"`
	Status string          `json:"status"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// Fetch retrieves one page through the rotating proxy.
func (c *Client) Fetch(ctx context.Context, target string, o *Options) (*Result, error) {
	var out Result
	err := c.post(ctx, "/v1/fetch", payload{URL: target, Options: c.applyDefaults(o)}, &out)
	return &out, err
}

// Extract pulls structured data from a page. fields maps a name to a CSS selector,
// an XPath string (leading "/"), or an object with css/xpath/attr/all. Pass nil
// fields and set Options.Prompt to use AI extraction instead.
func (c *Client) Extract(ctx context.Context, target string, fields map[string]any, o *Options) (*Result, error) {
	opts := c.applyDefaults(o)
	if fields != nil {
		opts.Fields = fields
	}
	var out Result
	err := c.post(ctx, "/v1/extract", payload{URL: target, Options: opts}, &out)
	return &out, err
}

// Batch fetches or extracts many URLs in one call.
func (c *Client) Batch(ctx context.Context, urls []string, o *Options) (*BatchResult, error) {
	var out BatchResult
	err := c.post(ctx, "/v1/batch", payload{URLs: urls, Options: c.applyDefaults(o)}, &out)
	return &out, err
}

// Rank scores a page's links by how relevant each is to query, so you can pick which
// sub-pages to fetch next instead of crawling everything. Set o.TopK to cap the list
// and o.Model ("fast"/"smart") to choose the backend. It is stateless: it ranks the
// links a page already has and never fetches them.
//
// AI transparency: if the ranking step fails, Rank still returns a result (no error)
// with the links in document order and RankResult.RankingError set — so check that
// field when the order matters. Requires the AI backend to be configured. Billed as one
// fetch plus the AI surcharge; a failed ranking bills only the fetch.
func (c *Client) Rank(ctx context.Context, target, query string, o *Options) (*RankResult, error) {
	var out RankResult
	err := c.post(ctx, "/v1/rank", rankPayload{URL: target, Query: query, Options: c.applyDefaults(o)}, &out)
	return &out, err
}

// BatchIter fetches or extracts any number of URLs, auto-chunking into BatchMaxURLs-sized
// Batch calls and invoking yield for each Result in input order as chunks complete — the
// streaming counterpart to Batch, so you can feed a slice of any size without holding
// every response in memory or tripping the 20-URL cap. yield returns false to stop early
// (the range-over-func convention, so this composes with `for r := range ...` on Go 1.23+).
// o applies to every URL, exactly like Batch. A failed chunk stops iteration and returns
// its error (the same *APIError as Batch).
//
//	err := c.BatchIter(ctx, urls, &scrapeland.Options{Fields: f}, func(r scrapeland.Result) bool {
//		save(r); return true
//	})
func (c *Client) BatchIter(ctx context.Context, urls []string, o *Options, yield func(Result) bool) error {
	for i := 0; i < len(urls); i += BatchMaxURLs {
		end := min(i+BatchMaxURLs, len(urls))
		res, err := c.Batch(ctx, urls[i:end], o)
		if err != nil {
			return err
		}
		for _, r := range res.Results {
			if !yield(r) {
				return nil
			}
		}
	}
	return nil
}

// Search runs a web search and returns organic results.
func (c *Client) Search(ctx context.Context, query string, o *Options) (*SearchResult, error) {
	var out SearchResult
	err := c.post(ctx, "/v1/search", payload{Q: query, Options: c.applyDefaults(o)}, &out)
	return &out, err
}

// SubmitJob queues an async job and returns its record.
func (c *Client) SubmitJob(ctx context.Context, jobType string, params map[string]any) (*Job, error) {
	body := map[string]any{"type": jobType}
	for k, v := range params {
		body[k] = v
	}
	var out Job
	err := c.post(ctx, "/v1/jobs", body, &out)
	return &out, err
}

// GetJob returns one job by id.
func (c *Client) GetJob(ctx context.Context, id string) (*Job, error) {
	var out Job
	err := c.get(ctx, "/v1/jobs/"+url.PathEscape(id), &out)
	return &out, err
}

// Jobs lists recent jobs. limit <= 0 uses the server default.
func (c *Client) Jobs(ctx context.Context, limit int) ([]Job, error) {
	path := "/v1/jobs"
	if limit > 0 {
		path += "?limit=" + strconv.Itoa(limit)
	}
	var out struct {
		Jobs []Job `json:"jobs"`
	}
	err := c.get(ctx, path, &out)
	return out.Jobs, err
}

// Capabilities reports what this deployment supports. No API key required.
func (c *Client) Capabilities(ctx context.Context) (map[string]any, error) {
	var out map[string]any
	err := c.get(ctx, "/v1/capabilities", &out)
	return out, err
}

// Account returns the calling key's plan, quota and usage.
func (c *Client) Account(ctx context.Context) (map[string]any, error) {
	var out map[string]any
	err := c.get(ctx, "/v1/account", &out)
	return out, err
}

// ---- transport ------------------------------------------------------------

func (c *Client) post(ctx context.Context, path string, body any, out any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("scrapeland: encoding request: %w", err)
	}
	return c.do(ctx, http.MethodPost, path, buf, out)
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodGet, path, nil, out)
}

func (c *Client) do(ctx context.Context, method, path string, body []byte, out any) error {
	var lastErr error
	delay := c.Backoff
	if delay <= 0 {
		delay = 500 * time.Millisecond
	}
	for attempt := 0; attempt <= c.Retries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
			delay *= 2
		}
		// Rebuild the reader each attempt: a consumed body cannot be replayed.
		var rdr io.Reader
		if body != nil {
			rdr = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, c.APIBase+path, rdr)
		if err != nil {
			return fmt.Errorf("scrapeland: building request: %w", err)
		}
		req.Header.Set("X-Api-Key", c.APIKey)
		req.Header.Set("Accept", "application/json")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := c.HTTP.Do(req)
		if err != nil {
			lastErr = err
			continue // network failure: a retry gets a different exit IP
		}
		respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}
		if resp.StatusCode >= 400 {
			var ra time.Duration
			if secs, e := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); e == nil && secs > 0 {
				ra = time.Duration(secs) * time.Second
			}
			lastErr = &APIError{Status: resp.StatusCode, Endpoint: path, Body: truncate(string(respBody), 512), RetryAfter: ra}
			if retryStatuses[resp.StatusCode] {
				continue
			}
			return lastErr // 4xx other than 429 will not improve on retry
		}
		if out != nil {
			if err := json.Unmarshal(respBody, out); err != nil {
				return fmt.Errorf("scrapeland: decoding %s response: %w", path, err)
			}
		}
		return nil
	}
	return lastErr
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
