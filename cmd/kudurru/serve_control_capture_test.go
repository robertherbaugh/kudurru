package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptrace"
	"sync"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/api"
)

// The real server sends HTTP 100 only when the settings handler reads its body,
// after complete-publication capture, cookie authentication and CSRF admission.
// Hold that body across a distinct source publication, then send it once. The
// transport has no reusable connection or GetBody with which to replay it.
func controlMountedHeldSettings(t *testing.T, client *http.Client, base, csrf string, publish func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	transport := &http.Transport{ExpectContinueTimeout: time.Second}
	defer transport.CloseIdleConnections()
	heldClient := &http.Client{Transport: transport, Timeout: client.Timeout, CheckRedirect: client.CheckRedirect}
	reader, writer := io.Pipe()
	defer func() { _ = reader.Close() }()
	defer func() { _ = writer.Close() }()
	body := []byte(`{"control":{"blockedQTypes":["aaaa"]}}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, base+"/settings", reader)
	if err != nil {
		t.Fatal("held control request construction failed")
	}
	req.ContentLength = int64(len(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Kudurru-CSRF", csrf)
	req.Header.Set("Expect", "100-continue")
	for _, cookie := range client.Jar.Cookies(req.URL) {
		req.AddCookie(cookie)
	}
	captured := make(chan struct{})
	var once sync.Once
	req = req.WithContext(httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{Got100Continue: func() {
		once.Do(func() { close(captured) })
	}}))
	type result struct {
		response *http.Response
		err      error
	}
	done := make(chan result, 1)
	go func() {
		response, err := heldClient.Do(req)
		done <- result{response: response, err: err}
	}()
	joined := false
	defer func() {
		cancel()
		_ = writer.Close()
		_ = reader.Close()
		if !joined {
			select {
			case got := <-done:
				if got.response != nil {
					_ = got.response.Body.Close()
				}
			case <-time.After(5 * time.Second):
				t.Error("held control HTTP request did not join cancellation")
			}
		}
	}()
	select {
	case <-captured:
	case got := <-done:
		joined = true
		if got.response != nil {
			_ = got.response.Body.Close()
		}
		t.Fatal("held control request completed before original capture")
	case <-ctx.Done():
		t.Fatal("held control request did not acknowledge original capture")
	}
	publish()
	// The original credential remains valid in a fresh publication: refusal of
	// the held request must come from its original source, not session revocation.
	read, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/auth/me", nil)
	if err != nil {
		t.Fatal("held control credential observation construction failed")
	}
	read.Header.Set("Cookie", req.Header.Get("Cookie"))
	observed, err := heldClient.Do(read)
	if err != nil {
		t.Fatal("held control credential observation transport failed")
	}
	_, _ = io.Copy(io.Discard, observed.Body)
	_ = observed.Body.Close()
	if observed.StatusCode != http.StatusOK {
		t.Fatalf("original control credential became invalid: status=%d", observed.StatusCode)
	}
	if _, err := io.Copy(writer, bytes.NewReader(body)); err != nil || writer.Close() != nil {
		t.Fatal("held control request body delivery failed")
	}
	select {
	case got := <-done:
		joined = true
		if got.err != nil || got.response == nil {
			t.Fatal("held control HTTP transport failed")
		}
		defer func() { _ = got.response.Body.Close() }()
		var failure struct{ Error api.ErrorBody }
		if json.NewDecoder(io.LimitReader(got.response.Body, 2<<20)).Decode(&failure) != nil ||
			got.response.StatusCode != http.StatusServiceUnavailable || failure.Error.Key != api.KeyUnavailable {
			t.Fatalf("superseded control request: status=%d original_publication_refused=%t", got.response.StatusCode, failure.Error.Key == api.KeyUnavailable)
		}
	case <-ctx.Done():
		t.Fatal("held control refusal exceeded original request deadline")
	}
}
