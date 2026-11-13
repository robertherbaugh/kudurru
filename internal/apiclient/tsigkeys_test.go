package apiclient

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestTSIGKeyCommandShapeAndSingleAttempt(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Idempotency-Key") != "" || r.Method != http.MethodPost {
			t.Error("stored TSIG key command was not a plain single POST")
		}
		http.Redirect(w, r, "/api/v1/tsigkeys", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	c, err := New(server.URL, Options{Token: "kdr_test"})
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []struct{ op, name string }{{"create", "named."}, {"replace", ""}, {"revoke", "a/b"}, {"delete", "k."}, {"reseal", strings.Repeat("a", 255)}} {
		if _, _, err := c.TSIGKeyCommand(t.Context(), bad.op, bad.name, []byte(`{}`)); err == nil {
			t.Fatalf("%s %q was accepted", bad.op, bad.name)
		}
	}
	if _, _, err := c.TSIGKeyCommand(t.Context(), "reseal", "k.", make([]byte, maxTSIGKeyRequest+1)); err == nil {
		t.Fatal("an oversized body was accepted")
	}
	if calls.Load() != 0 {
		t.Fatal("a locally refused command was sent")
	}
	// A redirect is not followed: the command is sent once and its 307 is an
	// error answer, never a second POST.
	if _, _, err := c.TSIGKeyCommand(t.Context(), "reseal", "k.", []byte(`{"uid":"u","resourceVersion":"1"}`)); !IsStatus(err, http.StatusTemporaryRedirect) {
		t.Fatalf("redirect was not refused: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("command was sent %d times", calls.Load())
	}
}
