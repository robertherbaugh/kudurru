package dnsdist

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAPIStatusNumericBackendIDs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/servers/localhost" || r.Header.Get("X-API-Key") != "fixture-key" {
			t.Errorf("unexpected API request: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"daemon_type":"dnsdist","version":"2.1.2","servers":[{"id":0,"name":"node-a","address":"10.0.0.11:5353","state":"up"},{"id":1,"name":"node-b","address":"10.0.0.12:5353","state":"UP"},{"id":2,"name":"node-c","address":"10.0.0.13:5353","state":"down"}]}`)
	}))
	defer srv.Close()
	c := APIClient{BaseURL: srv.URL, Key: "fixture-key"}
	st, err := c.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Servers) != 3 || st.Servers[1].ID != 1 || st.UpCount() != 2 {
		t.Fatalf("unexpected backend status: %+v", st)
	}
}
