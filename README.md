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

// Or describe what you want and let the model find it.
out, err = c.Extract(ctx, "https://example.com/product/1", nil, &scrapeland.Options{
	Prompt: "the product name, price in USD, and whether it is in stock",
})

// Many URLs in one call.
batch, err := c.Batch(ctx, []string{"https://a.com", "https://b.com"}, nil)

// Web search.
hits, err := c.Search(ctx, "golang web scraping", &scrapeland.Options{Count: 10})
```

## Configuration

`New` reads the environment when arguments are omitted:

| Setting | Option | Environment | Default |
| --- | --- | --- | --- |
| API key | first arg | `SCRAPELAND_API_KEY` | required |
| Gateway | `WithGateway` | `SCRAPELAND_GATEWAY` | `http://gateway.scrape.land:8080` |
| Data API | `WithAPIBase` | `SCRAPELAND_API_BASE` | `https://scrape.land` |

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
if errors.As(err, &apiErr) && apiErr.Status == http.StatusPaymentRequired {
	// out of credit
}
```

`429` and `5xx` are retried automatically (2 extra attempts by default, with
exponential backoff). Each retry rotates to a different exit IP, which is often
enough on its own. Other `4xx` responses are returned immediately — they will not
improve on a retry.

Every call that touches the network takes a `context.Context` and honours
cancellation, including between retries.

## License

MIT
