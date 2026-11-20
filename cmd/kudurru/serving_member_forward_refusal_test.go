package main

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/api"
	clusterreplica "github.com/robertherbaugh/kudurru/internal/cluster/replica"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

func TestMemberSelfError(t *testing.T) {
	const notDone = "the password was not changed"
	testMemberForwardRefusals(t, notDone, func(err error, sent bool) error {
		return memberSelfError(err, sent, notDone, "the password change's outcome is unknown")
	})
}

func TestMemberLogoutError(t *testing.T) {
	testMemberForwardRefusals(t, "the session was not revoked", func(err error, sent bool) error {
		return memberLogoutError(err, sent, false)
	})
}

func TestObjectForwardError(t *testing.T) {
	testMemberForwardRefusals(t, "the write was not applied", objectForwardError)
}

func testMemberForwardRefusals(t *testing.T, notDone string, classify func(error, bool) error) {
	t.Helper()
	const authority = "cluster authority changed"
	const noReplay = "do not replay this request"
	for _, testCase := range []struct {
		name   string
		cause  error
		sent   bool
		status int
		key    string
		detail string
	}{
		{"member token or writable role rejected", clusterreplica.ErrUnauthorized, false, http.StatusServiceUnavailable, api.KeyUnavailable, authority},
		{"fenced", replication.ErrFenced, false, http.StatusServiceUnavailable, api.KeyUnavailable, authority},
		{"different authority", replication.ErrCAS, false, http.StatusServiceUnavailable, api.KeyUnavailable, authority},
		{"read only", replication.ErrReadOnly, false, http.StatusServiceUnavailable, api.KeyUnavailable, authority},
		{"promotion conflict", replication.ErrPromotionConflict, false, http.StatusServiceUnavailable, api.KeyUnavailable, authority},
		{"expired command", clusterreplica.ErrExpired, false, http.StatusServiceUnavailable, api.KeyUnavailable, "the enrolled primary is unavailable"},
		{"definite refusal", errors.New("dial failed"), false, http.StatusServiceUnavailable, api.KeyUnavailable, "the enrolled primary is unavailable"},
		{"forbidden", clusterreplica.ErrForbidden, false, http.StatusForbidden, api.KeyForbidden, "the primary refused this credential"},
		{"denied", servingauth.ErrDenied, false, http.StatusForbidden, api.KeyForbidden, "the primary refused this credential"},
		{"post-answer authority change", replication.ErrCAS, true, http.StatusServiceUnavailable, "completion_unknown", noReplay},
		{"post-answer expiry", clusterreplica.ErrExpired, true, http.StatusServiceUnavailable, "completion_unknown", noReplay},
		{"post-answer failure", errors.New("local response failed"), true, http.StatusServiceUnavailable, "completion_unknown", noReplay},
		{"post-answer authorization failure", clusterreplica.ErrUnauthorized, true, http.StatusServiceUnavailable, "completion_unknown", noReplay},
		{"post-answer forbidden", clusterreplica.ErrForbidden, true, http.StatusForbidden, api.KeyForbidden, "the primary refused this credential"},
		{"unverifiable response", errors.Join(clusterreplica.ErrOutcomeUnknown, clusterreplica.ErrUnauthorized), false, http.StatusServiceUnavailable, "completion_unknown", noReplay},
		{"unverifiable forbidden response", errors.Join(clusterreplica.ErrOutcomeUnknown, clusterreplica.ErrForbidden), false, http.StatusServiceUnavailable, "completion_unknown", noReplay},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			answer := servingHTTPAuthError(classify(testCase.cause, testCase.sent))
			if answer.Status != testCase.status || answer.Key != testCase.key || !strings.Contains(answer.Message, testCase.detail) || answer.RetryAfter != 0 {
				t.Fatalf("wrong refusal: %+v", answer)
			}
			if testCase.key == api.KeyUnavailable && !strings.Contains(answer.Message, notDone) ||
				testCase.key == "completion_unknown" && strings.Contains(answer.Message, notDone) {
				t.Fatalf("wrong certainty: %+v", answer)
			}
		})
	}
}
