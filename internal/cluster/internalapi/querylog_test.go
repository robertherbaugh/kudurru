package internalapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/querylog/sqlitelog"
)

func TestQuerylogGuardAndBounds(t *testing.T) {
	creds, err := NewCredentials(CredentialOptions{Key: key(7), NodeName: "node-a"})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	s, err := NewServer(ServerOptions{Credentials: creds, Querylog: func(context.Context, QuerylogRequest) (*QuerylogResponse, error) {
		calls++
		return &QuerylogResponse{Queries: &sqlitelog.QueryPage{}}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		q          string
		authorized bool
		status     int
	}{
		{q: `{"kind":"queries"}`, status: 401},
		{q: `{"kind":"queries","n":401}`, authorized: true, status: 400},
		{q: `{"kind":"queries","filter":{"limit":1001}}`, authorized: true, status: 400},
		{q: `{"kind":"queries","scope":"cluster"}`, authorized: true, status: 400},
		{q: `bad`, authorized: true, status: 400},
		{q: `{"kind":"queries","filter":{"limit":1}}`, authorized: true, status: 200},
	} {
		r := httptest.NewRequest(http.MethodGet, PathQuerylog+"?"+url.Values{"q": {tc.q}}.Encode(), nil)
		if tc.authorized {
			r.Header.Set("Authorization", "Bearer "+creds.tokensToTry()[0])
		}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("%s: status %d, want %d: %s", tc.q, w.Code, tc.status, w.Body.String())
		}
		if w.Code == 200 {
			var body QuerylogResponse
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if calls != 1 {
		t.Fatalf("invalid or unauthorized reads reached callback: %d", calls)
	}
}
