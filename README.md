# scrapeland-go

Go client for the [scrape.land](https://scrape.land) web data extraction API.
Standard library only — no dependencies.

```sh
go get github.com/scrape-land/scrapeland-go
```

## Two ways to use it

### 1. As an `*http.Client` — every request exits a fresh IP

This is usually what you want. Nothing else in your code changes: hand it to any
library that takes an `*http.Client`.

```go
package main

import (
	"fmt"
	"io"

	"github.com/scrape-land/scrapeland-go"
)

func main() {
	c, err := scrapeland.New("pb_live_YOURKEY")
	if err != nil {
		panic(err)
	}

	res, err := c.HTTPClient(nil).Get("https://api.ipify.org")
	if err != nil {
		panic(err)
	}
	defer res.Body.Close()

	body, _ := io.ReadAll(res.Body)
	fmt.Println("exit IP:", string(body)) // different on every call
}
```

Route through a specific country, or pin a sticky session so a sequence of
requests shares one exit IP:

```go
us := c.HTTPClient(&scrapeland.Options{Country: "us"})
job := c.HTTPClient(&scrapeland.Options{Session: "checkout-42"})
```

### 2. As a data API — get structured data back

```go
ctx := context.Background()

// Clean Markdown, ready for an LLM: navigation, headers, footers and sidebars
// stripped, tables preserved, relative links rewritten to absolute.
page, err := c.Fetch(ctx, "https://example.com", &scrapeland.Options{Format: "markdown"})
fmt.Println(page.Markdown)

// CSS or XPath selectors.
out, err := c.Extract(ctx, "https://example.com/product/1", map[string]any{
	"title": "h1",
	"price": ".price",
	"specs": map[string]any{"css": "li.spec", "all": true},
}, nil)
fmt.Println(out.Data["title"])

// Or describe what you want and let the model find it. AI extraction (Prompt,
// Schema, ExtractType) needs a Scale plan or above; selector Fields work on every
// plan. Check c.Capabilities(ctx) if you need to branch.
out, err = c.Extract(ctx, "https://example.com/product/1", nil, &scrapeland.Options{
	Prompt: "the product name, price in USD, and whether it is in stock",
})

// Asking a QUESTION rather than listing fields? Structured: scrapeland.Bool(false)
// answers in prose via out.Answer, leaving out.Data nil — so you never have to guess
// the key names a model invented. Schema and ExtractType pin a JSON shape and so
// cannot be combined with it (the API returns 400).
out, err = c.Extract(ctx, "https://scrape.land/", nil, &scrapeland.Options{
	Prompt:     "what's this business about?",
	Structured: scrapeland.Bool(false),
})
fmt.Println(out.Answer)

// JavaScript pages: Render drives a real browser. It bills 5 request-units
// instead of 1 (10 with a screenshot, 1 with BlockResources) and downloads
// everything the page asks for — measured at 1–300x the bytes (median ~17x,
// image-heavy shops worst). Set
// BlockResources unless you need a screenshot or the images: fonts, images and
// media are skipped and the DOM your selectors read is identical.
out, err = c.Extract(ctx, "https://example.com/product/1",
	map[string]any{"title": "h1"},
	&scrapeland.Options{Render: true, WaitFor: "h1", BlockResources: true})

// Many URLs in one call.
batch, err := c.Batch(ctx, []string{"https://a.com", "https://b.com"}, nil)

// Web search.
hits, err := c.Search(ctx, "golang web scraping", &scrapeland.Options{Count: 10})

// Rank a page's links by relevance — pick what to fetch next instead of crawling all.
r, err := c.Rank(ctx, "https://news.example/", "articles about interest rates",
	&scrapeland.Options{TopK: 10})
for _, link := range r.Links {
	fmt.Println(link.Score, link.URL, "-", link.Reason)
}
// AI transparency: on an AI failure Rank returns NO error — links come back in
// document order with r.RankingError set, so check it when the order matters.
if r.RankingError != "" {
	log.Println("ranking degraded:", r.RankingError)
}

// BatchIter: any number of URLs, auto-chunked into <=20-URL calls, in order.
err = c.BatchIter(ctx, allURLs, &scrapeland.Options{Fields: map[string]any{"title": "h1"}},
	func(res scrapeland.Result) bool {
		if res.Error != "" {
			log.Println("failed", res.URL, res.Error)
		} else {
			save(res.URL, res.Data)
		}
		return true // return false to stop early
	})
```

## Configuration

`New` reads the environment when arguments are omitted:

| Setting | Option | Environment | Default |
| --- | --- | --- | --- |
| API key | first arg | `SCRAPELAND_API_KEY` | required |
| Gateway | `WithGateway` | `SCRAPELAND_GATEWAY` | `http://gateway.scrape.land:8080` |
| Data API | `WithAPIBase` | `SCRAPELAND_API_BASE` | `https://scrape.land` |

**Blocked port?** The gateway also answers on `gateway.scrape.land:443`, for
corporate, university and hotel networks that filter high ports. Identical
behaviour and billing — only the port differs, and the scheme stays `http://` with
your key in the username:

```go
c, err := scrapeland.New("pb_live_YOURKEY",
	scrapeland.WithGateway("http://gateway.scrape.land:443"))
```

Client-wide defaults, overridable per request:

```go
c, err := scrapeland.New("pb_live_YOURKEY",
	scrapeland.WithCountry("de"),
	scrapeland.WithMaxLatency(3000),
	scrapeland.WithRetries(3),
)
```

## Errors

Non-2xx responses come back as `*APIError`, carrying the status and the server's
message — check for a 402 to detect an exhausted quota:

```go
var apiErr *scrapeland.APIError
if errors.As(err, &apiErr) {
	switch {
	case apiErr.IsPaymentRequired(): // out of credit / quota
	case apiErr.IsRateLimited():     time.Sleep(apiErr.RetryAfter) // honors Retry-After
	case apiErr.IsUnauthorized():    // bad or revoked key
	case apiErr.IsServer():          // transient 5xx
	}
	// or check apiErr.Status directly; apiErr.Body has the server's message.
}
```

`*APIError` carries `Status`, `Body`, `RetryAfter`, and `IsBadRequest` / `IsUnauthorized`
/ `IsPaymentRequired` / `IsNotFound` / `IsRateLimited` / `IsServer` helpers.

`429` and `5xx` are retried automatically (2 extra attempts by default, with
exponential backoff). Each retry rotates to a different exit IP, which is often
enough on its own. Other `4xx` responses are returned immediately — they will not
improve on a retry.

Every call that touches the network takes a `context.Context` and honours
cancellation, including between retries.

A `413` means the response was larger than your plan's cap; the message names the
size, the limit and the plan. It is refused whole — never truncated — and never
billed. A `503` means we are momentarily at capacity (usually every browser slot is
busy on a `Render`); it carries `Retry-After` and is not billed either.

## Plan limits

`c.Account(ctx)` returns your own numbers; these are the shapes to code against.

| | Free | Starter | Growth | Scale | Business+ | Pay as you go |
| --- | --- | --- | --- | --- | --- | --- |
| Rate limit | 5/s | 50/s | 100/s | 200/s | 1,000/s | none |
| Max response | 2 MB | 2 MB | 5 MB | 10 MB | 25 MB | 5 MB |
| AI extraction | — | — | — | yes | yes | — |

Past your included requests, overage draws on **prepaid credit** and floors at
zero — at zero you get a `402`, never an invoice after the fact.

## License

MIT
