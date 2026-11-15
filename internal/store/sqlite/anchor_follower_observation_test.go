package sqlite

import (
	"errors"
	"reflect"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/trustanchors"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/retirement"
)

func TestAnchorFollowerObservationAuthenticatesWithoutCanonicalProgress(t *testing.T) {
	for _, kind := range []string{"normal", "revoke", "expired-normal", "expired-revoke", "invalid-signature", "malformed"} {
		t.Run(kind, func(t *testing.T) {
			primary, _, proof := anchorLocalTestManaged(t)
			follower, source := anchorLocalTestFollower(t, primary)
			if err := anchorLocalTestInstall(anchorLocalTestFinite(t.Context(), t), t, follower, primary); err != nil {
				t.Fatal(err)
			}
			clock, err := trustanchors.NewSessionClock()
			if err != nil {
				t.Fatal(err)
			}
			d, err := follower.CaptureAnchorDenial(anchorLocalTestFinite(t.Context(), t), proof.next.Version(), clock)
			if err != nil {
				t.Fatal(err)
			}
			message := anchorTestSigned(t, proof.key, proof.signer, kind == "revoke" || kind == "expired-revoke")
			if kind == "expired-normal" || kind == "expired-revoke" {
				for _, rr := range message.Answer {
					rr.Header().Ttl = 0
				}
			}
			raw := anchorPollTestPack(t, message)
			switch kind {
			case "invalid-signature":
				raw[len(raw)-1] ^= 1
			case "malformed":
				raw = raw[:11]
			}
			stamp, err := d.SampleReceipt()
			if err != nil {
				t.Fatal(err)
			}
			before, image, authority := source.cursor.Load(), imageOf(t, follower), follower.Authority()
			branch, verified, err := d.ObserveReceived(raw, stamp)
			switch kind {
			case "normal":
				if err != nil || branch != AnchorPollNormal || verified != nil {
					t.Fatal("authenticated normal was not an explicit proof-free result", branch, err)
				}
			case "revoke":
				if err != nil || branch != AnchorPollRevoke || verified == nil {
					t.Fatal("authenticated revoke lost its original deny-only proof", branch, err)
				}
			default:
				if err == nil || branch != AnchorPollInvalid || verified != nil {
					t.Fatal("failed input was classified as a successful observation", branch, err)
				}
				if (kind == "expired-normal" || kind == "expired-revoke") && !errors.Is(err, trustanchors.ErrExpired) {
					t.Fatal("signed zero-TTL receipt lost actual lifetime refusal", err)
				}
			}
			if source.closed.Load() != (kind == "expired-revoke") || follower.anchorBlocked.Load() != (kind == "expired-revoke") {
				t.Fatal("known expired denial did not exclusively close local admission")
			}
			if source.cursor.Load() != before || !sameAnchorLocal(before.local, anchorLocalTestRead(t, follower)) {
				t.Fatal("classification created durable owner or local state")
			}
			if branch, next, err := d.ObserveReceived(raw, stamp); !errors.Is(err, retirement.ErrUsed) || branch != AnchorPollInvalid || next != nil {
				t.Fatal("input was authenticated again", branch, err)
			}
			if _, err := d.VerifyReceived(raw, stamp); !errors.Is(err, retirement.ErrUsed) {
				t.Fatal("legacy raw path bypassed shared consumption", err)
			}
			if _, err := d.Verify(message); !errors.Is(err, retirement.ErrUsed) {
				t.Fatal("legacy parsed path bypassed shared consumption", err)
			}
			if _, err := d.SampleReceipt(); !errors.Is(err, retirement.ErrUsed) {
				t.Fatal("consumed input was restamped", err)
			}
			if kind == "revoke" {
				completion, _, err := follower.CommitAnchorDenial(verified)
				if err != nil || completion.Outcome() != AnchorCommitted || len(anchorLocalTestRead(t, follower).record.Denied) != 1 {
					t.Fatal("explicit revoke did not commit original local denial", err)
				}
			}
			after := imageOf(t, follower)
			if !image.Generation().Same(after.Generation()) || !reflect.DeepEqual(image.Payload, after.Payload) || !reflect.DeepEqual(authority, follower.Authority()) {
				t.Fatal("follower observation advanced canonical image/checkpoint/authority")
			}
		})
	}
}

func TestAnchorFollowerObservationStampIdentityAndLegacyContract(t *testing.T) {
	primary, _, proof := anchorLocalTestManaged(t)
	follower, source := anchorLocalTestFollower(t, primary)
	if err := anchorLocalTestInstall(anchorLocalTestFinite(t.Context(), t), t, follower, primary); err != nil {
		t.Fatal(err)
	}
	capture := func() *AnchorDenial {
		t.Helper()
		d, err := follower.CaptureAnchorDenial(anchorLocalTestFinite(t.Context(), t), proof.next.Version(), proof.clock)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	d, foreign := capture(), capture()
	stamp, err := d.SampleReceipt()
	if err != nil {
		t.Fatal(err)
	}
	foreignStamp, err := foreign.SampleReceipt()
	if err != nil {
		t.Fatal(err)
	}
	raw := anchorPollTestPack(t, anchorTestSigned(t, proof.key, proof.signer, false))
	before := source.cursor.Load()
	for _, wrong := range []AnchorReceiptStamp{{}, foreignStamp, {cell: &anchorReceiptStamp{original: d.cell, at: stamp.cell.at}}} {
		if branch, verified, err := d.ObserveReceived(raw, wrong); !errors.Is(err, replication.ErrInvalid) || branch != AnchorPollInvalid || verified != nil {
			t.Fatal("foreign or substituted cell became an authenticated observation", branch, err)
		}
	}
	copyOfStamp := stamp
	if branch, verified, err := d.ObserveReceived(raw, copyOfStamp); err != nil || branch != AnchorPollNormal || verified != nil {
		t.Fatal("invalid cells consumed the rightful receipt", branch, err)
	}
	if verified, err := foreign.VerifyReceived(raw, foreignStamp); !errors.Is(err, replication.ErrInvalid) || verified != nil {
		t.Fatal("legacy normal response began granting a denial proof", err)
	}
	if branch, _, err := foreign.ObserveReceived(raw, foreignStamp); !errors.Is(err, retirement.ErrUsed) || branch != AnchorPollInvalid {
		t.Fatal("legacy normal input was reusable through observation API", err)
	}
	if source.cursor.Load() != before || !sameAnchorLocal(before.local, anchorLocalTestRead(t, follower)) {
		t.Fatal("stamp refusal or normal classification changed local state")
	}
	for _, invalid := range []*AnchorDenial{nil, {}} {
		if branch, verified, err := invalid.ObserveReceived(raw, stamp); !errors.Is(err, replication.ErrInvalid) || branch != AnchorPollInvalid || verified != nil {
			t.Fatal("empty denial became an authenticated observation", branch, err)
		}
	}
}
