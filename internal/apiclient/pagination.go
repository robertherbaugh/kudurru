package apiclient

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"
)

// ListPage is the list envelope {items, continue, took_ms}.
type ListPage struct {
	Items    []json.RawMessage `json:"items"`
	Continue string            `json:"continue,omitempty"`
	TookMS   float64           `json:"took_ms,omitempty"`
}

// Pages walks a keyset-paginated list endpoint, calling fn for every page
// until the server returns an empty "continue" or fn returns an error.
// pageSize sets ?limit= (0 leaves it to the server). The query is not
// modified.
func (c *Client) Pages(ctx context.Context, path string, query url.Values, pageSize int, fn func(ListPage) error) error {
	q := cloneValues(query)
	if pageSize > 0 {
		q.Set("limit", strconv.Itoa(pageSize))
	}
	seen := map[string]bool{}
	for {
		var page ListPage
		if err := c.Get(ctx, path, q, &page); err != nil {
			return err
		}
		if err := fn(page); err != nil {
			return err
		}
		// A server echoing the same token forever must not loop forever.
		if page.Continue == "" || seen[page.Continue] {
			return nil
		}
		seen[page.Continue] = true
		q.Set("continue", page.Continue)
	}
}

// ListAll collects every item of a paginated list endpoint.
func (c *Client) ListAll(ctx context.Context, path string, query url.Values, pageSize int) ([]json.RawMessage, error) {
	var out []json.RawMessage
	err := c.Pages(ctx, path, query, pageSize, func(p ListPage) error {
		out = append(out, p.Items...)
		return nil
	})
	return out, err
}

func cloneValues(v url.Values) url.Values {
	out := make(url.Values, len(v))
	for k, vs := range v {
		out[k] = append([]string(nil), vs...)
	}
	return out
}
