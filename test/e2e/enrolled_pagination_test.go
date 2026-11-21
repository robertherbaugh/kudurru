package e2e

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"
)

func TestEnrolledRecordSetNamesRestartsPaginationAfterContinuationConflict(t *testing.T) {
	t.Parallel()
	const discardedToken = "discarded+token/="
	const freshToken = "fresh+token/="
	var (
		requests []string
		mu       sync.Mutex
	)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mu.Lock()
		requests = append(requests, request.Method+" "+request.URL.RequestURI())
		call := len(requests)
		mu.Unlock()
		switch call {
		case 1:
			writeRecordSetPage(t, writer, "discarded", discardedToken)
		case 2:
			writePaginationConflict(t, writer)
		case 3:
			writeRecordSetPage(t, writer, "fresh-first", freshToken)
		case 4:
			writeRecordSetPage(t, writer, "fresh-last", "")
		default:
			t.Errorf("unexpected request %s %s", request.Method, request.URL.RequestURI())
			http.Error(writer, "unexpected request", http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	names, err := enrolledRecordSetNames(t.Context(), paginationClient(server))
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]bool{"fresh-first": true, "fresh-last": true}; !maps.Equal(names, want) {
		t.Fatalf("names = %#v, want %#v", names, want)
	}
	wantRequests := []string{
		"GET /objects/dnsrecordsets?limit=50",
		"GET /objects/dnsrecordsets?limit=50&continue=discarded%2Btoken%2F%3D",
		"GET /objects/dnsrecordsets?limit=50",
		"GET /objects/dnsrecordsets?limit=50&continue=fresh%2Btoken%2F%3D",
	}
	mu.Lock()
	gotRequests := append([]string(nil), requests...)
	mu.Unlock()
	if !slices.Equal(gotRequests, wantRequests) {
		t.Fatalf("requests = %#v, want %#v", gotRequests, wantRequests)
	}
}

func TestEnrolledRecordSetNamesDoesNotRestartTerminalResponses(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name               string
		responses          []int
		paginationConflict bool
	}{
		{name: "unrelated continuation conflict", responses: []int{http.StatusOK, http.StatusConflict}},
		{name: "first page pagination conflict", responses: []int{http.StatusConflict}, paginationConflict: true},
		{name: "service unavailable", responses: []int{http.StatusServiceUnavailable}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var (
				calls int
				mu    sync.Mutex
			)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				mu.Lock()
				calls++
				call := calls
				mu.Unlock()
				if call > len(test.responses) {
					t.Errorf("unexpected request %d: %s %s", call, request.Method, request.URL.RequestURI())
					http.Error(writer, "unexpected request", http.StatusInternalServerError)
					return
				}
				if request.Method != http.MethodGet {
					t.Errorf("method = %s, want GET", request.Method)
				}
				switch test.responses[call-1] {
				case http.StatusOK:
					writeRecordSetPage(t, writer, "first", "next")
				case http.StatusConflict:
					if test.paginationConflict {
						writePaginationConflict(t, writer)
					} else {
						writePaginationError(t, writer, http.StatusConflict, "conflict", "a different conflict", "resolve the conflicting object")
					}
				default:
					writePaginationError(t, writer, http.StatusServiceUnavailable, "unavailable", "try later", "")
				}
			}))
			defer server.Close()

			names, err := enrolledRecordSetNames(t.Context(), paginationClient(server))
			if err == nil {
				t.Fatal("expected terminal error")
			}
			if names != nil {
				t.Fatalf("names = %#v, want nil on error", names)
			}
			mu.Lock()
			gotCalls := calls
			mu.Unlock()
			if gotCalls != len(test.responses) {
				t.Fatalf("calls = %d, want %d", gotCalls, len(test.responses))
			}
		})
	}
}

func TestEnrolledRecordSetNamesCapsRequestsAcrossPagination(t *testing.T) {
	t.Parallel()
	const wantCalls = 101
	var (
		calls int
		mu    sync.Mutex
	)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mu.Lock()
		calls++
		call := calls
		mu.Unlock()
		if call > wantCalls {
			http.Error(writer, "exceeded scan budget", http.StatusInternalServerError)
			return
		}
		wantQuery := "limit=50"
		if call%2 == 0 {
			wantQuery += "&continue=stale%2Btoken%2F%3D"
		}
		if request.Method != http.MethodGet || request.URL.RawQuery != wantQuery {
			t.Errorf("request = %s %s", request.Method, request.URL.RequestURI())
		}
		if call%2 == 0 {
			writePaginationConflict(t, writer)
			return
		}
		writeRecordSetPage(t, writer, "discarded", "stale+token/=")
	}))
	defer server.Close()

	names, err := enrolledRecordSetNames(t.Context(), paginationClient(server))
	if err == nil {
		t.Fatal("expected request-cap error")
	}
	if names != nil {
		t.Fatalf("names = %#v, want nil on error", names)
	}
	mu.Lock()
	gotCalls := calls
	mu.Unlock()
	if gotCalls != wantCalls {
		t.Fatalf("calls = %d, want %d", gotCalls, wantCalls)
	}
}

func paginationClient(server *httptest.Server) *staticProcessClient {
	return &staticProcessClient{client: &http.Client{Timeout: 5 * time.Second}, base: server.URL}
}

func writeRecordSetPage(t *testing.T, writer http.ResponseWriter, name, cont string) {
	t.Helper()
	writer.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(writer).Encode(map[string]any{
		"items":    []any{map[string]any{"metadata": map[string]any{"name": name}}},
		"continue": cont,
	}); err != nil {
		t.Error(err)
	}
}

func writePaginationConflict(t *testing.T, writer http.ResponseWriter) {
	t.Helper()
	writePaginationError(t, writer, http.StatusConflict, "conflict", "object pagination generation or query changed", "restart pagination without continue")
}

func writePaginationError(t *testing.T, writer http.ResponseWriter, status int, key, message, hint string) {
	t.Helper()
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	if err := json.NewEncoder(writer).Encode(map[string]any{"error": map[string]any{"key": key, "message": message, "hint": hint}}); err != nil {
		t.Error(err)
	}
}
