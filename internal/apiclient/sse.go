package apiclient

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Event is one server-sent event.
type Event struct {
	// ID is the "id:" field; the client resumes after it with
	// Last-Event-ID when it reconnects.
	ID string
	// Event is the "event:" field ("" means "message").
	Event string
	// Data is the "data:" lines joined with "\n".
	Data []byte
}

// SubscribeOptions configure Subscribe.
type SubscribeOptions struct {
	// LastEventID resumes a stream after this id.
	LastEventID string
	// Reconnect re-opens the stream after a disconnect (with
	// Last-Event-ID) until ctx ends. Without it Subscribe returns on the
	// first disconnect.
	Reconnect bool
	// RetryDelay is the initial reconnect delay (default 1 s; the
	// server's "retry:" field overrides it). It doubles up to MaxDelay.
	RetryDelay time.Duration
	// MaxDelay caps the reconnect delay (default 30 s).
	MaxDelay time.Duration
}

// ErrStopSubscription may be returned by a Subscribe handler to end the
// subscription without an error.
var ErrStopSubscription = errors.New("apiclient: stop subscription")

// Subscribe opens an SSE stream (GET with Accept: text/event-stream) and
// calls fn for every event. It returns when ctx ends (nil), when fn
// returns an error (ErrStopSubscription is reported as nil), or on a
// disconnect when opts.Reconnect is false. HTTP errors when opening the
// stream are returned as *Error and never retried, except 429/502/503/504
// while reconnecting.
func (c *Client) Subscribe(ctx context.Context, path string, query url.Values, opts SubscribeOptions, fn func(Event) error) error {
	delay := opts.RetryDelay
	if delay <= 0 {
		delay = time.Second
	}
	maxDelay := opts.MaxDelay
	if maxDelay <= 0 {
		maxDelay = 30 * time.Second
	}
	lastID := opts.LastEventID
	everConnected := false
	for {
		connected, got, retry, err := c.subscribeOnce(ctx, path, query, &lastID, fn)
		everConnected = everConnected || connected
		var he *handlerError
		if errors.As(err, &he) {
			if errors.Is(he.err, ErrStopSubscription) {
				return nil
			}
			return he.err
		}
		switch {
		case ctx.Err() != nil:
			return nil
		case errors.Is(err, ErrStopSubscription):
			return nil
		case !opts.Reconnect:
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		case !connected && !isRetryable(err, everConnected):
			return err
		}
		if retry > 0 {
			delay = retry
		}
		if got {
			// The stream delivered events; restart the back-off.
			delay = max(retry, opts.RetryDelay, 100*time.Millisecond)
		}
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		}
		delay = min(delay*2, maxDelay)
	}
}

// isRetryable decides whether a failure to (re)open the stream is worth
// another attempt: transient HTTP statuses always are, other HTTP errors
// (401, 403, 404) never, and transport errors only once the stream has
// worked before (a bad URL is reported at once).
func isRetryable(err error, everConnected bool) bool {
	var e *Error
	if errors.As(err, &e) {
		switch e.StatusCode {
		case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return true
		}
		return false
	}
	return everConnected
}

type handlerError struct{ err error }

func (h *handlerError) Error() string { return h.err.Error() }
func (h *handlerError) Unwrap() error { return h.err }

// subscribeOnce runs one connection. It reports whether the stream opened,
// whether any event arrived and the last "retry:" value. A clean end of the
// stream is io.EOF; a handler error is wrapped in *handlerError.
func (c *Client) subscribeOnce(ctx context.Context, path string, query url.Values, lastID *string, fn func(Event) error) (bool, bool, time.Duration, error) {
	h := http.Header{}
	h.Set("Cache-Control", "no-cache")
	if *lastID != "" {
		h.Set("Last-Event-ID", *lastID)
	}
	resp, err := c.Raw(ctx, Request{Path: path, Query: query, Accept: "text/event-stream", Header: h, NoTimeout: true})
	if err != nil {
		return false, false, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := CheckResponse(resp); err != nil {
		return false, false, 0, err
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "text/event-stream") {
		return false, false, 0, fmt.Errorf("apiclient: %s answered %q, not text/event-stream", path, ct)
	}
	var (
		got   bool
		retry time.Duration
	)
	err = ParseSSE(resp.Body, func(ev Event, r time.Duration) error {
		if r > 0 {
			retry = r
		}
		if ev.Data == nil && ev.Event == "" && ev.ID == "" {
			return nil
		}
		if ev.ID != "" {
			*lastID = ev.ID
		}
		got = true
		if err := fn(ev); err != nil {
			return &handlerError{err}
		}
		return nil
	})
	if err == nil {
		err = io.EOF
	}
	return true, got, retry, err
}

// ParseSSE reads an event stream (WHATWG HTML §9.2) from r and calls fn
// for every dispatched event with the latest "retry:" value. It returns
// fn's error or the read error (nil at a clean EOF).
func ParseSSE(r io.Reader, fn func(Event, time.Duration) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4<<20)
	var (
		ev      Event
		data    []string
		hasData bool
		retry   time.Duration
	)
	dispatch := func() error {
		if hasData {
			ev.Data = []byte(strings.Join(data, "\n"))
		}
		if !hasData && ev.ID == "" && ev.Event == "" && retry == 0 {
			return nil
		}
		err := fn(ev, retry)
		ev, data, hasData = Event{}, data[:0], false
		return err
	}
	for sc.Scan() {
		line := strings.TrimSuffix(sc.Text(), "\r")
		if line == "" {
			if err := dispatch(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue // comment / keep-alive
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "data":
			data = append(data, value)
			hasData = true
		case "event":
			ev.Event = value
		case "id":
			if !strings.ContainsRune(value, 0) {
				ev.ID = value
			}
		case "retry":
			if ms, err := strconv.Atoi(value); err == nil && ms >= 0 {
				retry = time.Duration(ms) * time.Millisecond
			}
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	// A final event without a trailing blank line is discarded per spec.
	return nil
}
