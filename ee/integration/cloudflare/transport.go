package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/robertherbaugh/kudurru/ee/integration/host"
	"github.com/robertherbaugh/kudurru/ee/integration/ledger"
	"github.com/robertherbaugh/kudurru/ee/integration/plan"
)

type envelope[T any] struct {
	Success    *bool    `json:"success"`
	Result     T        `json:"result"`
	ResultInfo pageInfo `json:"result_info"`
}

func (e envelope[T]) valid() error {
	if e.Success == nil || !*e.Success {
		return ErrInvalid
	}
	return nil
}

type pageInfo struct {
	Page       *int `json:"page"`
	PerPage    *int `json:"per_page"`
	Count      *int `json:"count"`
	TotalCount *int `json:"total_count"`
	TotalPages *int `json:"total_pages"`
}

func (p pageInfo) valid(page, size, count int) error {
	if p.Page == nil || *p.Page != page || p.PerPage == nil || *p.PerPage != size || p.Count == nil || *p.Count != count || p.TotalCount == nil || *p.TotalCount < 0 || *p.TotalCount > maxRecords || count > size {
		return ErrInvalid
	}
	pages := max(1, (*p.TotalCount+size-1)/size)
	if pages > maxPages || page > pages || p.TotalPages != nil && *p.TotalPages != pages || count != min(size, max(0, *p.TotalCount-(page-1)*size)) {
		return ErrInvalid
	}
	return nil
}

type requestTransport struct {
	host          host.OwnerHost
	base          http.RoundTripper
	origin, token string
}

func (t *requestTransport) CloseIdleConnections() {
	if closer, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

func (t *requestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	origin, err := url.Parse(t.origin)
	if err != nil || req.URL.Scheme != origin.Scheme || req.URL.Host != origin.Host || !strings.HasPrefix(req.URL.Path, origin.Path) || req.URL.User != nil {
		return nil, ErrInvalid
	}
	var response *http.Response
	mutating := req.Method != http.MethodGet && req.Method != http.MethodHead
	err = t.host.Request(req.Context(), "cloudflare", "all", mutating, func(ctx context.Context) error {
		request := req.Clone(ctx)
		request.Header = req.Header.Clone()
		request.Close = true
		request.Header.Set("Authorization", "Bearer "+t.token)
		for _, name := range []string{"X-Auth-Key", "X-Auth-Email", "X-Auth-User-Service-Key"} {
			request.Header.Del(name)
		}
		var err error
		response, err = t.base.RoundTrip(request)
		if err != nil {
			return safe(ctx, err)
		}
		if response == nil || response.Body == nil {
			return ErrInvalid
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, plan.MaxSnapshotBytes+1))
		closeErr := response.Body.Close()
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return statusFailure(response.StatusCode, response.Header.Get("Retry-After"))
		}
		if readErr != nil || closeErr != nil {
			return &host.Failure{Kind: host.FailureTemporary}
		}
		if len(body) > plan.MaxSnapshotBytes {
			return ErrInvalid
		}
		var env struct {
			Success *bool             `json:"success"`
			Result  json.RawMessage   `json:"result"`
			Errors  []json.RawMessage `json:"errors"`
		}
		if json.Unmarshal(body, &env) != nil || env.Success == nil || !*env.Success || len(env.Errors) > 0 || len(env.Result) == 0 || bytes.Equal(bytes.TrimSpace(env.Result), []byte("null")) {
			return ErrInvalid
		}
		response.Body = io.NopCloser(bytes.NewReader(body))
		response.ContentLength = int64(len(body))
		return nil
	})
	if err != nil {
		if errors.Is(err, ledger.ErrBudget) {
			return nil, &host.Failure{Kind: host.FailureRateLimited, RetryAfter: 5 * time.Minute}
		}
		return nil, safe(req.Context(), err)
	}
	return response, nil
}
func statusFailure(code int, retry string) error {
	kind := host.FailurePermanent
	if code == 429 {
		kind = host.FailureRateLimited
	} else if code == 408 || code == 409 || code >= 500 {
		kind = host.FailureTemporary
	}
	after := time.Duration(0)
	if seconds, err := strconv.ParseUint(retry, 10, 32); err == nil {
		if seconds <= 900 {
			after = time.Duration(seconds) * time.Second
		} else {
			after = 15 * time.Minute
		}
	} else if at, err := http.ParseTime(retry); err == nil {
		after = max(0, min(15*time.Minute, time.Until(at)))
	}
	return &host.Failure{Kind: kind, RetryAfter: after}
}
func safe(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var failure *host.Failure
	if errors.As(err, &failure) {
		return failure
	}
	if errors.Is(err, host.ErrFenced) || errors.Is(err, host.ErrUnavailable) {
		return host.ErrFenced
	}
	if errors.Is(err, ErrInvalid) {
		return &host.Failure{Kind: host.FailurePermanent}
	}
	return &host.Failure{Kind: host.FailureTemporary}
}
