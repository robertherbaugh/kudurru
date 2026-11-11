package httpbatch_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/querylog"
	"github.com/robertherbaugh/kudurru/internal/querylog/sinks/httpbatch"
	"github.com/robertherbaugh/kudurru/internal/querylog/sinks/sinktest"
)

// The body of every test batch. It stands for query contents: no error may
// repeat it.
const batchBody = `{"domain":"private-name.example","client":"192.0.2.7"}` + "\n"

// credentials is a Resolver over a map that tests change, with a call count.
type credentials struct {
	mu     sync.Mutex
	values map[string]string
	errs   map[string]error
	calls  int
}

func (c *credentials) resolve(_ context.Context, ref config.CredentialRef) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if err := c.errs[ref.File]; err != nil {
		return nil, err
	}
	value, ok := c.values[ref.File]
	if !ok {
		return nil, errors.New("credential is unavailable")
	}
	return []byte(value), nil
}

func (c *credentials) set(file, value string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.values[file] = value
}

func file(path string) *config.CredentialRef { return &config.CredentialRef{File: path} }

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func open(t *testing.T, o httpbatch.Options) *httpbatch.Client {
	t.Helper()
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	c, err := httpbatch.New(o)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Open(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

// retryable reports how the sink host would treat err: whether the batch
// may be written again, and after which wait the destination asked for.
// The host is the only reader of the mark, so the test asks it the same way.
func retryable(t *testing.T, err error) (time.Duration, bool) {
	t.Helper()
	return querylog.RetryableWait(err)
}

// TestPostSortsAnswersIntoStoredRetryableAndDropped: the three outcomes of
// D371 for every kind of answer, and no error repeats the response body,
// the request body or the credential.
func TestPostSortsAnswersIntoStoredRetryableAndDropped(t *testing.T) {
	double := sinktest.New(t, http.StatusOK, false)
	creds := &credentials{values: map[string]string{"/run/secrets/p": "s3cr3t-password\n"}}
	client := open(t, httpbatch.Options{
		Destination: "ClickHouse", URL: mustURL(t, double.URL+"/insert?table=q"), Header: http.Header{"Content-Type": {"application/x-ndjson"}},
		Username: "kudurru", Password: file("/run/secrets/p"), Resolve: creds.resolve,
		Detail: func(h http.Header) string { return h.Get("X-Detail") },
		Now:    func() time.Time { return time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC) },
	})
	const leak = "Cannot parse row: private-name.example from 192.0.2.7"
	for _, tc := range []struct {
		name      string
		answer    sinktest.Answer
		stored    bool
		again     bool
		wait      time.Duration
		wantError string
	}{
		{name: "200", answer: sinktest.Answer{Status: 200}, stored: true},
		{name: "204", answer: sinktest.Answer{Status: 204}, stored: true},
		{name: "500", answer: sinktest.Answer{Status: 500, Body: leak}, again: true, wantError: "ClickHouse answered HTTP 500"},
		{name: "502", answer: sinktest.Answer{Status: 502}, again: true, wantError: "ClickHouse answered HTTP 502"},
		{name: "503 with seconds", answer: sinktest.Answer{Status: 503, Header: http.Header{"Retry-After": {"7"}}}, again: true, wait: 7 * time.Second, wantError: "HTTP 503"},
		{name: "503 with a date", answer: sinktest.Answer{Status: 503, Header: http.Header{"Retry-After": {"Tue, 10 Nov 2026 12:00:20 GMT"}}}, again: true, wait: 20 * time.Second, wantError: "HTTP 503"},
		{name: "503 with a past date", answer: sinktest.Answer{Status: 503, Header: http.Header{"Retry-After": {"Tue, 10 Nov 2026 11:00:00 GMT"}}}, again: true, wantError: "HTTP 503"},
		{name: "429", answer: sinktest.Answer{Status: 429, Header: http.Header{"Retry-After": {"2"}}}, again: true, wait: 2 * time.Second, wantError: "HTTP 429"},
		{name: "408", answer: sinktest.Answer{Status: 408}, again: true, wantError: "HTTP 408"},
		{name: "400", answer: sinktest.Answer{Status: 400, Body: leak, Header: http.Header{"X-Detail": {"exception code 27"}}}, wantError: "ClickHouse answered HTTP 400 (exception code 27)"},
		{name: "404", answer: sinktest.Answer{Status: 404, Body: leak}, wantError: "ClickHouse answered HTTP 404"},
		{name: "413", answer: sinktest.Answer{Status: 413}, wantError: "HTTP 413"},
		{name: "401 with an unchanged credential", answer: sinktest.Answer{Status: 401}, wantError: "ClickHouse answered HTTP 401"},
		{name: "403 with an unchanged credential", answer: sinktest.Answer{Status: 403}, wantError: "ClickHouse answered HTTP 403"},
		{name: "301", answer: sinktest.Answer{Status: 301, Header: http.Header{"Location": {"http://127.0.0.1:1/elsewhere"}}}, wantError: "HTTP 301"},
	} {
		double.Answer(func(int, sinktest.Request) sinktest.Answer { return tc.answer })
		err := client.Post(t.Context(), []byte(batchBody))
		if tc.stored {
			if err != nil {
				t.Errorf("%s: %v", tc.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.wantError) {
			t.Errorf("%s: error %v, want %q", tc.name, err, tc.wantError)
			continue
		}
		if wait, again := retryable(t, err); again != tc.again || wait != tc.wait {
			t.Errorf("%s: retryable %v after %s, want %v after %s", tc.name, again, wait, tc.again, tc.wait)
		}
		for _, secret := range []string{"private-name", "192.0.2.7", "s3cr3t", "czNjcjN0", "insert?table"} {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("%s: the error repeats %q: %v", tc.name, secret, err)
			}
		}
	}
	requests := double.Requests()
	if len(requests) != 15 {
		t.Fatalf("%d requests for 15 batches: a redirect was followed or a batch was sent twice", len(requests))
	}
	for i, r := range requests {
		if r.Method != http.MethodPost || r.Path != "/insert" || r.Query.Get("table") != "q" || string(r.Body) != batchBody || r.Compressed {
			t.Fatalf("request %d: %s %s %q", i+1, r.Method, r.Path, r.Body)
		}
		if got := r.Header.Get("Authorization"); got != sinktest.Basic("kudurru", "s3cr3t-password") {
			t.Fatalf("request %d: the password was not sent as basic authentication with its trailing newline removed", i+1)
		}
		if r.Header.Get("Content-Type") != "application/x-ndjson" || r.Header.Get("Content-Encoding") != "" {
			t.Fatalf("request %d headers: %v", i+1, r.Header)
		}
		if !r.CloseAfter {
			t.Fatalf("request %d kept its connection: every batch must use its own", i+1)
		}
	}
}

// TestPostNeverConnectedMayBeSentAgain: with no connection nothing was sent,
// so the batch may be written again.
func TestPostNeverConnectedMayBeSentAgain(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	client := open(t, httpbatch.Options{Destination: "Loki", URL: mustURL(t, "http://"+address+"/loki/api/v1/push")})
	err = client.Post(t.Context(), []byte(batchBody))
	if err == nil || !strings.Contains(err.Error(), "Loki: no connection to http://"+address+", nothing was sent") {
		t.Fatalf("refused connection: %v", err)
	}
	if _, again := retryable(t, err); !again {
		t.Fatal("a batch that was never sent must be retryable")
	}
	if strings.Contains(err.Error(), "private-name") || strings.Contains(err.Error(), "/loki/api") {
		t.Fatalf("the error repeats the request: %v", err)
	}
}

// TestPostWithoutAnAnswerIsNeverSentAgain: once the request was sent, a lost
// connection or a deadline leaves the outcome unknown, and the batch is
// dropped instead of written twice.
func TestPostWithoutAnAnswerIsNeverSentAgain(t *testing.T) {
	double := sinktest.New(t, http.StatusOK, false)
	client := open(t, httpbatch.Options{Destination: "ClickHouse", URL: mustURL(t, double.URL)})

	double.Answer(func(int, sinktest.Request) sinktest.Answer { return sinktest.Answer{Drop: true} })
	err := client.Post(t.Context(), []byte(batchBody))
	if err == nil || !strings.Contains(err.Error(), "gave no answer after the batch was sent, whether it was stored is unknown") {
		t.Fatalf("dropped connection: %v", err)
	}
	if _, again := retryable(t, err); again {
		t.Fatal("a batch with an unknown outcome was marked retryable")
	}

	release := make(chan struct{})
	defer close(release)
	double.Answer(func(int, sinktest.Request) sinktest.Answer { return sinktest.Answer{Hang: release} })
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	began := time.Now()
	err = client.Post(ctx, []byte(batchBody))
	if err == nil || !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "whether it was stored is unknown") {
		t.Fatalf("stalled answer: %v", err)
	}
	if _, again := retryable(t, err); again {
		t.Fatal("a batch that timed out after it was sent was marked retryable")
	}
	if took := time.Since(began); took > 5*time.Second {
		t.Fatalf("the attempt outlived its deadline by %s", took)
	}
	if got := len(double.Requests()); got != 2 {
		t.Fatalf("%d requests for 2 batches", got)
	}
}

// TestPostRereadsARefusedCredential: a 401 or 403 means nothing was stored.
// The reference is read again; a changed value makes the batch retryable
// and is used from then on, an unchanged or unreadable one drops it.
func TestPostRereadsARefusedCredential(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		double := sinktest.New(t, http.StatusNoContent, false)
		creds := &credentials{values: map[string]string{"/run/secrets/token": "old-token"}, errs: map[string]error{}}
		accepted := "Bearer new-token"
		double.Answer(func(_ int, r sinktest.Request) sinktest.Answer {
			if r.Header.Get("Authorization") != accepted {
				return sinktest.Answer{Status: status}
			}
			return sinktest.Answer{}
		})
		client := open(t, httpbatch.Options{Destination: "Loki", URL: mustURL(t, double.URL), Bearer: file("/run/secrets/token"), Resolve: creds.resolve})

		// Unchanged: dropped, one request.
		err := client.Post(t.Context(), []byte(batchBody))
		var refused *httpbatch.StatusError
		if !errors.As(err, &refused) || refused.Status != status || refused.Reloaded {
			t.Fatalf("%d with an unchanged token: %v", status, err)
		}
		if _, again := retryable(t, err); again {
			t.Fatalf("%d with an unchanged token was marked retryable", status)
		}
		// The reference cannot be read: dropped, and the old token stays.
		creds.errs["/run/secrets/token"] = errors.New("credential is unavailable")
		if err := client.Post(t.Context(), []byte(batchBody)); err == nil {
			t.Fatal("refused credential with an unreadable reference")
		} else if _, again := retryable(t, err); again {
			t.Fatal("an unreadable reference made the batch retryable")
		}
		delete(creds.errs, "/run/secrets/token")
		// Rotated: the same batch may be sent again, and is then stored.
		creds.set("/run/secrets/token", "new-token")
		err = client.Post(t.Context(), []byte(batchBody))
		if !errors.As(err, &refused) || !refused.Reloaded || !strings.Contains(err.Error(), "the credential was read again") {
			t.Fatalf("%d with a rotated token: %v", status, err)
		}
		if _, again := retryable(t, err); !again {
			t.Fatalf("%d with a rotated token must be retryable", status)
		}
		if strings.Contains(err.Error(), "new-token") {
			t.Fatalf("the error repeats the token: %v", err)
		}
		if err := client.Post(t.Context(), []byte(batchBody)); err != nil {
			t.Fatalf("the rotated token was not used: %v", err)
		}
		requests := double.Requests()
		if len(requests) != 4 {
			t.Fatalf("%d requests, want 4", len(requests))
		}
		for i, want := range []string{"Bearer old-token", "Bearer old-token", "Bearer old-token", "Bearer new-token"} {
			if got := requests[i].Header.Get("Authorization"); got != want {
				t.Fatalf("request %d presented another credential than expected", i+1)
			}
		}
		// Open read it once, each refusal once more, a stored batch never.
		if creds.calls != 4 {
			t.Fatalf("the reference was read %d times, want 4", creds.calls)
		}
	}
}

// largeBatch is a batch of about 4 MB, more than the socket buffers of a
// loopback connection hold, as a batch of 10,000 rows can be.
func largeBatch() []byte { return bytes.Repeat([]byte(batchBody), 4<<20/len(batchBody)) }

// TestPostLargeBatchSeesARefusalAtTheHeaders: a destination that refuses a
// request at its headers and resets the connection without reading the body
// must still be heard. A large batch asks for "100 Continue" first, so the
// body is never written into the reset: the 403 reads the credential again,
// the 503 is repeated, and nothing is reported as an unknown outcome.
func TestPostLargeBatchSeesARefusalAtTheHeaders(t *testing.T) {
	double := sinktest.New(t, http.StatusNoContent, false)
	creds := &credentials{values: map[string]string{"/run/secrets/token": "old-token"}}
	client := open(t, httpbatch.Options{Destination: "Loki", URL: mustURL(t, double.URL), Bearer: file("/run/secrets/token"), Resolve: creds.resolve})
	body := largeBatch()

	// The double waits to see whether the body follows the answer.
	double.AnswerBeforeBody(http.StatusForbidden, 300*time.Millisecond)
	creds.set("/run/secrets/token", "new-token")
	err := client.Post(t.Context(), body)
	var refused *httpbatch.StatusError
	if !errors.As(err, &refused) || refused.Status != http.StatusForbidden || !refused.Reloaded {
		t.Fatalf("early 403 with a rotated token: %v", err)
	}
	if _, again := retryable(t, err); !again {
		t.Fatal("early 403 with a rotated token must be retryable")
	}
	reqs := double.WaitRequests(t, 1)
	if reqs[0].SentAfterAnswer != 0 {
		t.Errorf("the client wrote %d bytes of the body into a refusal", reqs[0].SentAfterAnswer)
	}
	if reqs[0].Header.Get("Expect") != "100-continue" {
		t.Errorf("a large batch did not ask for 100 Continue")
	}

	// An immediate reset, repeated: the status arrives every time.
	double.AnswerBeforeBody(http.StatusServiceUnavailable, 0)
	lost := 0
	for range 20 {
		err := client.Post(t.Context(), body)
		if !errors.As(err, &refused) || refused.Status != http.StatusServiceUnavailable {
			lost++
			t.Logf("early 503 was lost: %v", err)
		} else if _, again := retryable(t, err); !again {
			t.Errorf("early 503 must be retryable: %v", err)
		}
	}
	if lost > 0 {
		t.Errorf("the early 503 was lost in %d of 20 attempts", lost)
	}

	// A destination that accepts reads the body after "100 Continue".
	double.AnswerBeforeBody(0, 0)
	if err := client.Post(t.Context(), body); err != nil {
		t.Fatalf("large batch: %v", err)
	}
	reqs = double.Requests()
	if last := reqs[len(reqs)-1]; !bytes.Equal(last.Body, body) {
		t.Fatalf("large batch arrived with %d bytes, want %d", len(last.Body), len(body))
	}
	// A small batch fits the socket buffers and does not wait for an answer.
	if err := client.Post(t.Context(), []byte(batchBody)); err != nil {
		t.Fatal(err)
	}
	reqs = double.Requests()
	if got := reqs[len(reqs)-1].Header.Get("Expect"); got != "" {
		t.Fatalf("a small batch sent Expect: %q", got)
	}
}

// TestPostLargeBatchToAServerThatIgnoresExpect: a server that never answers
// "100 Continue" still gets the body, after a short wait.
func TestPostLargeBatchToAServerThatIgnoresExpect(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	got := make(chan int, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		r := bufio.NewReader(conn)
		req, err := http.ReadRequest(r)
		if err != nil {
			got <- -1
			return
		}
		// Read the body without sending 100 Continue.
		n, _ := io.Copy(io.Discard, req.Body)
		got <- int(n)
		_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
	}()
	client := open(t, httpbatch.Options{Destination: "ClickHouse", URL: mustURL(t, "http://"+listener.Addr().String()+"/")})
	body := largeBatch()
	if err := client.Post(t.Context(), body); err != nil {
		t.Fatalf("server without 100 Continue: %v", err)
	}
	if n := <-got; n != len(body) {
		t.Fatalf("the server read %d bytes, want %d", n, len(body))
	}
}

// TestOpenReadsCredentialsAndRefusesUnusableOnes: Open contacts only the
// credential source, and its errors name the reference kind, never a value.
func TestOpenReadsCredentialsAndRefusesUnusableOnes(t *testing.T) {
	target := mustURL(t, "https://127.0.0.1:1/")
	creds := &credentials{values: map[string]string{ //nolint:gosec // G101: made-up credential fixtures
		"/empty": " \n", "/multiline": "line-one\nline-two", "/spaces": "tok en", "/padding": "=abc", "/token": "abc.DEF-123_~+/==", "/not-pem": "not a certificate",
	}}
	// A combined system bundle is larger than any value the node's
	// resolver returns; the error says so instead of "no PEM certificate".
	creds.values["/bundle"] = strings.Repeat("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n", 2000)
	for name, tc := range map[string]struct {
		opts httpbatch.Options
		want string
	}{
		"missing password": {httpbatch.Options{Username: "u", Password: file("/missing")}, "ClickHouse: password: credential is unavailable"},
		"empty password":   {httpbatch.Options{Username: "u", Password: file("/empty")}, "ClickHouse: password: the reference holds no usable value"},
		"missing token":    {httpbatch.Options{Bearer: file("/missing")}, "ClickHouse: bearer token: credential is unavailable"},
		"multiline token":  {httpbatch.Options{Bearer: file("/multiline")}, "ClickHouse: bearer token: the reference does not hold a bearer token"},
		"token with space": {httpbatch.Options{Bearer: file("/spaces")}, "the reference does not hold a bearer token"},
		"token of padding": {httpbatch.Options{Bearer: file("/padding")}, "the reference does not hold a bearer token"},
		"missing CA":       {httpbatch.Options{CA: file("/missing")}, "ClickHouse: CA certificates: credential is unavailable"},
		"CA without PEM":   {httpbatch.Options{CA: file("/not-pem")}, "ClickHouse: CA certificates: the reference holds no PEM certificate"},
		"CA too large":     {httpbatch.Options{CA: file("/bundle")}, "ClickHouse: CA certificates: the reference holds more than 64 KiB; name a file with only the private CA"},
	} {
		tc.opts.Destination, tc.opts.URL, tc.opts.Resolve, tc.opts.Logger = "ClickHouse", target, creds.resolve, slog.New(slog.DiscardHandler)
		client, err := httpbatch.New(tc.opts)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		err = client.Open(t.Context())
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
		if err != nil && (strings.Contains(err.Error(), "line-one") || strings.Contains(err.Error(), "tok en")) {
			t.Errorf("%s: the error repeats the value: %v", name, err)
		}
		if err := client.Post(t.Context(), []byte(batchBody)); err == nil || !strings.Contains(err.Error(), "the sink is not open") {
			t.Errorf("%s: a client that did not open sent a batch: %v", name, err)
		}
	}
	// A valid token opens without any connection: port 1 is never dialed.
	ok, err := httpbatch.New(httpbatch.Options{Destination: "Loki", URL: target, Bearer: file("/token"), Resolve: creds.resolve, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	if err := ok.Open(t.Context()); err != nil {
		t.Fatalf("a valid token was refused: %v", err)
	}
	ok.Close()

	for name, opts := range map[string]httpbatch.Options{
		"no URL":                   {},
		"another scheme":           {URL: mustURL(t, "ftp://example.com/")},
		"credential in the URL":    {URL: mustURL(t, "https://user:pass@example.com/")},
		"basic and bearer":         {URL: target, Username: "u", Password: file("/p"), Bearer: file("/t"), Resolve: creds.resolve},
		"password without a user":  {URL: target, Password: file("/p"), Resolve: creds.resolve},
		"reference without a host": {URL: target, Bearer: file("/t")},
	} {
		if _, err := httpbatch.New(opts); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// TestPostOverTLSTrustsOnlyTheNamedCA: with a caRef the bundle replaces the
// system trust store; without it a private CA is refused before anything
// is sent.
func TestPostOverTLSTrustsOnlyTheNamedCA(t *testing.T) {
	double := sinktest.New(t, http.StatusOK, true)
	other := sinktest.New(t, http.StatusOK, true)
	creds := &credentials{values: map[string]string{"/ca": string(double.CertPEM()), "/token": "tls-token"}}
	trusted := open(t, httpbatch.Options{Destination: "ClickHouse", URL: mustURL(t, double.URL), CA: file("/ca"), Bearer: file("/token"), Resolve: creds.resolve})
	if err := trusted.Post(t.Context(), []byte(batchBody)); err != nil {
		t.Fatalf("TLS with the named CA: %v", err)
	}
	if err := trusted.Post(t.Context(), []byte(batchBody)); err != nil {
		t.Fatalf("second TLS batch: %v", err)
	}
	if got := double.Requests(); len(got) != 2 || got[0].Header.Get("Authorization") != "Bearer tls-token" || string(got[1].Body) != batchBody {
		t.Fatalf("TLS requests: %s", sinktest.Describe(got))
	}

	system := open(t, httpbatch.Options{Destination: "ClickHouse", URL: mustURL(t, other.URL), Bearer: file("/token"), Resolve: creds.resolve})
	err := system.Post(t.Context(), []byte(batchBody))
	if err == nil || !strings.Contains(err.Error(), "nothing was sent") || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("TLS without the CA: %v", err)
	}
	if _, again := retryable(t, err); !again {
		t.Fatal("a failed handshake sent nothing and must be retryable")
	}
	if len(other.Requests()) != 0 {
		t.Fatal("a request reached a server whose certificate was not trusted")
	}
}

// TestPostNeverFollowsARedirect: the credential and the batch stay with the
// configured host.
func TestPostNeverFollowsARedirect(t *testing.T) {
	elsewhere := sinktest.New(t, http.StatusOK, false)
	double := sinktest.New(t, http.StatusOK, false)
	for _, status := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		double.Answer(func(int, sinktest.Request) sinktest.Answer {
			return sinktest.Answer{Status: status, Header: http.Header{"Location": {elsewhere.URL + "/collect"}}}
		})
		creds := &credentials{values: map[string]string{"/token": "redirect-token"}}
		client := open(t, httpbatch.Options{Destination: "Loki", URL: mustURL(t, double.URL), Bearer: file("/token"), Resolve: creds.resolve})
		err := client.Post(t.Context(), []byte(batchBody))
		var refused *httpbatch.StatusError
		if !errors.As(err, &refused) || refused.Status != status {
			t.Fatalf("%d: %v", status, err)
		}
		if _, again := retryable(t, err); again {
			t.Fatalf("%d was marked retryable", status)
		}
	}
	if got := elsewhere.Requests(); len(got) != 0 {
		t.Fatalf("a redirect was followed: %s", sinktest.Describe(got))
	}
}

// TestPostCompressesWithGzip: the body arrives intact with its encoding
// declared, across batches of different sizes.
func TestPostCompressesWithGzip(t *testing.T) {
	double := sinktest.New(t, http.StatusOK, false)
	client := open(t, httpbatch.Options{Destination: "ClickHouse", URL: mustURL(t, double.URL), Gzip: true, Username: "default"})
	large := strings.Repeat(batchBody, 2000)
	for _, body := range []string{batchBody, large, batchBody} {
		if err := client.Post(t.Context(), []byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	requests := double.Requests()
	if len(requests) != 3 {
		t.Fatalf("%d requests", len(requests))
	}
	for i, want := range []string{batchBody, large, batchBody} {
		r := requests[i]
		if !r.Compressed || r.Header.Get("Content-Encoding") != "gzip" || string(r.Body) != want {
			t.Fatalf("request %d: compressed %v, %d bytes, want %d", i+1, r.Compressed, len(r.Body), len(want))
		}
	}
	// A user without a password is sent as basic authentication too.
	if got := requests[0].Header.Get("Authorization"); got != sinktest.Basic("default", "") {
		t.Fatalf("user without a password: %q", got)
	}
}

// TestOpenWarnsAboutACredentialOverPlainHTTP names the risk once, without
// the credential; loopback and HTTPS stay quiet.
func TestOpenWarnsAboutACredentialOverPlainHTTP(t *testing.T) {
	creds := &credentials{values: map[string]string{"/token": "warn-token"}}
	for target, want := range map[string]bool{
		"http://loki.example:3100/": true, "http://127.0.0.1:3100/": false, "http://localhost:3100/": false, "http://[::1]:3100/": false, "https://loki.example/": false,
	} {
		var logs strings.Builder
		client, err := httpbatch.New(httpbatch.Options{Destination: "Loki", URL: mustURL(t, target), Bearer: file("/token"), Resolve: creds.resolve, Logger: slog.New(slog.NewTextHandler(&logs, nil))})
		if err != nil {
			t.Fatal(err)
		}
		if err := client.Open(t.Context()); err != nil {
			t.Fatal(err)
		}
		client.Close()
		if got := strings.Contains(logs.String(), "sends its credential over plain HTTP"); got != want {
			t.Errorf("%s: warned %v, want %v", target, got, want)
		}
		if strings.Contains(logs.String(), "warn-token") {
			t.Errorf("%s: the warning repeats the credential", target)
		}
	}
	// Without a credential there is nothing to warn about.
	var logs strings.Builder
	client, err := httpbatch.New(httpbatch.Options{Destination: "Loki", URL: mustURL(t, "http://loki.example:3100/"), Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Open(t.Context()); err != nil || logs.Len() != 0 {
		t.Fatalf("no credential: %v %s", err, logs.String())
	}
}
