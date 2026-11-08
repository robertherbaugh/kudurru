package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestTSIGKeyRoutesReceiptStatusesPermissionsAndNoFallback(t *testing.T) {
	secret := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x65}, 32))
	created := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	metadata := TSIGKeyMetadata{KeyName: "stored.", Algorithm: "hmac-sha256.", Source: "store", UID: "original-owner", ResourceVersion: "17", CreatedAt: &created}
	published := true
	calls := 0
	h := newHarness(t, func(o *Options) {
		o.TSIGKeyReads = func(context.Context) ([]TSIGKeyMetadata, error) { return []TSIGKeyMetadata{metadata}, nil }
		o.TSIGKeyCommand = func(_ context.Context, change TSIGKeyChange) (*TSIGKeyCommandReceipt, error) {
			calls++
			if change.KeyName != metadata.KeyName {
				t.Fatal("wrong canonical key target")
			}
			post := metadata
			if change.Operation == TSIGKeyReplace || change.Operation == TSIGKeyReseal {
				post.ResourceVersion = "18"
			}
			return &TSIGKeyCommandReceipt{Operation: change.Operation, Committed: true, Published: published, Removed: change.Operation == TSIGKeyRevoke, Key: post}, nil
		}
	})
	create := map[string]any{"keyName": "stored.", "algorithm": "hmac-sha256", "secret": secret}
	binding := map[string]any{"uid": metadata.UID, "resourceVersion": metadata.ResourceVersion}
	for _, user := range []string{"viewer", "auditor", "operator", "zoneadmin"} {
		for _, path := range []string{"/api/v1/tsigkeys", "/api/v1/tsigkeys/stored./replace", "/api/v1/tsigkeys/stored./revoke", "/api/v1/tsigkeys/stored./reseal"} {
			if got := h.do(http.MethodPost, path, user, create).Code; got != http.StatusForbidden {
				t.Fatalf("nonadmin TSIG route status=%d", got)
			}
		}
		if got := h.do(http.MethodGet, "/api/v1/tsigkeys", user, nil).Code; got != http.StatusForbidden {
			t.Fatalf("nonadmin metadata status=%d", got)
		}
	}
	if calls != 0 {
		t.Fatal("denied route invoked command")
	}
	for _, pub := range []bool{true, false} {
		published = pub
		for _, tc := range []struct {
			path      string
			operation TSIGKeyOperation
			body      map[string]any
		}{
			{"/api/v1/tsigkeys", TSIGKeyCreate, create},
			{"/api/v1/tsigkeys/stored./replace", TSIGKeyReplace, map[string]any{"uid": metadata.UID, "resourceVersion": metadata.ResourceVersion, "secret": secret}},
			{"/api/v1/tsigkeys/stored./revoke", TSIGKeyRevoke, binding},
			{"/api/v1/tsigkeys/stored./reseal", TSIGKeyReseal, binding},
		} {
			result := h.do(http.MethodPost, tc.path, "admin", tc.body)
			want := http.StatusAccepted
			if pub {
				want = http.StatusOK
				if tc.operation == TSIGKeyCreate {
					want = http.StatusCreated
				}
			}
			if result.Code != want {
				t.Fatalf("receipt status=%d want=%d", result.Code, want)
			}
			var receipt TSIGKeyCommandReceipt
			if json.Unmarshal(result.Body.Bytes(), &receipt) != nil || !receipt.Committed || receipt.Published != pub || receipt.Removed != (tc.operation == TSIGKeyRevoke) || receipt.Key.UID != metadata.UID {
				t.Fatal("receipt binding lost")
			}
			if strings.Contains(result.Body.String(), secret) || strings.Contains(result.Body.String(), "sealed") {
				t.Fatal("receipt exposed private material")
			}
		}
	}
	h.srv.opts.TSIGKeyCommand = nil
	if got := h.do(http.MethodPost, "/api/v1/tsigkeys", "admin", create).Code; got != http.StatusNotImplemented {
		t.Fatalf("missing durable capability status=%d", got)
	}
	if calls != 8 {
		t.Fatal("mutation was retried or unsupported route invoked fallback")
	}
	h.srv.opts.TSIGKeyReads = nil
	if got := h.do(http.MethodGet, "/api/v1/tsigkeys", "admin", nil).Code; got != http.StatusNotImplemented {
		t.Fatalf("missing read capability status=%d", got)
	}
}

func TestTSIGKeyRouteRejectsUnconfirmedReceiptWithoutRollbackClaim(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		o.TSIGKeyCommand = func(context.Context, TSIGKeyChange) (*TSIGKeyCommandReceipt, error) {
			return &TSIGKeyCommandReceipt{}, nil
		}
	})
	body := map[string]any{"keyName": "stored.", "algorithm": "hmac-sha256", "secret": base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{3}, 32))}
	result := h.do(http.MethodPost, "/api/v1/tsigkeys", "admin", body)
	if result.Code != http.StatusServiceUnavailable || strings.Contains(result.Body.String(), "rollback") || strings.Contains(result.Body.String(), "retry") || strings.Contains(result.Body.String(), `"committed":false`) {
		t.Fatal("unknown result promised rollback or replay")
	}
	operation := h.srv.api.OpenAPI().Paths["/tsigkeys"].Post
	if operation.Responses["201"] == nil || operation.Responses["202"] == nil || operation.Responses["501"] == nil {
		t.Fatal("receipt or capability status schema absent")
	}
	if operation.RequestBody == nil || operation.MaxBodyBytes != maxTSIGKeyBody || !operation.SkipValidateBody {
		t.Fatal("bounded safe body decoder not wired")
	}
}

func TestTSIGKeyRouteSanitizesUnexpectedBackendError(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		o.TSIGKeyCommand = func(context.Context, TSIGKeyChange) (*TSIGKeyCommandReceipt, error) {
			return nil, errors.New("private carrier and SQL detail")
		}
		o.TSIGKeyReads = func(context.Context) ([]TSIGKeyMetadata, error) {
			return nil, errors.New("private carrier and SQL detail")
		}
	})
	body := map[string]any{"keyName": "stored.", "algorithm": "hmac-sha256", "secret": base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{3}, 32))}
	for _, result := range []response{h.do(http.MethodPost, "/api/v1/tsigkeys", "admin", body), h.do(http.MethodGet, "/api/v1/tsigkeys", "admin", nil)} {
		if result.Code != http.StatusServiceUnavailable || strings.Contains(result.Body.String(), "private carrier") || strings.Contains(result.Body.String(), "retry") || strings.Contains(result.Body.String(), "rollback") {
			t.Fatal("unexpected backend error escaped safe uncertain-outcome contract")
		}
	}
}
