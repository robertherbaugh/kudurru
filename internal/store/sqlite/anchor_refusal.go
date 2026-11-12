package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"slices"

	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
)

// AnchorRefusalReason names why an install or resolution refused an image's
// different managed root-anchor owner (D369). It is a diagnostic class for
// operators, never a grant or a retry hint.
type AnchorRefusalReason string

// Refusal classes, in the order the admission rules check them.
const (
	// AnchorRefusedHistory: another cluster, history or initialization.
	AnchorRefusedHistory AnchorRefusalReason = "history"
	// AnchorRefusedPolicy: another policy, seed digest or trust domain.
	AnchorRefusedPolicy AnchorRefusalReason = "policy"
	// AnchorRefusedDormantOwner: this member holds managed history and the
	// image's owner is dormant (the primary never started managed anchors).
	AnchorRefusedDormantOwner AnchorRefusalReason = "dormant_owner"
	// AnchorRefusedRecoveryHistory: the image does not retain an anchors
	// recover record that this member accepted.
	AnchorRefusedRecoveryHistory AnchorRefusalReason = "recovery_history"
	// AnchorRefusedCarriedRevocation: the image's owner still trusts a key
	// that this member's owner revoked or that this member denied locally.
	AnchorRefusedCarriedRevocation AnchorRefusalReason = "carried_revocation"
	// AnchorRefusedNotSuccessor: any other owner that the rules refuse, such
	// as an install inside one term that is not a strict successor.
	AnchorRefusedNotSuccessor AnchorRefusalReason = "not_successor"
)

// AnchorRefusal is the refusal of an image's managed root-anchor owner. It
// matches accepted.ErrCAS (errors.Is), so every caller keeps the original
// refusal category; nothing was written. Keys lists the identities (trustanchors
// Key.ID) that the image's owner still trusts and this member knows to be
// revoked, for AnchorRefusedCarriedRevocation only.
type AnchorRefusal struct {
	Reason AnchorRefusalReason
	Keys   []string
}

func (e *AnchorRefusal) Error() string {
	return "store/sqlite: root anchor owner refused (" + string(e.Reason) + ")"
}

// Unwrap preserves the original refusal category.
func (e *AnchorRefusal) Unwrap() error { return accepted.ErrCAS }

// anchorInstallRefusal classifies an accepted.ErrCAS refusal of next by an
// install or resolution over before. Every other error, and a refusal that is
// already classified, is returned unchanged. It changes no admission rule: the
// caller has already refused.
func anchorInstallRefusal(before, next *anchorCursor, err error) error {
	var typed *AnchorRefusal
	if err == nil || !errors.Is(err, accepted.ErrCAS) || errors.As(err, &typed) {
		return err
	}
	reason := anchorRefusalReason(before, next)
	if reason == AnchorRefusedNotSuccessor && before != nil && before.local != nil && !before.awaiting && next != nil && !next.awaiting &&
		before.owner.Inspect().Phase == accepted.Managed && next.owner.Inspect().Phase == accepted.Managed {
		// The same coverage rule an install inside one term applies through
		// anchorSuccessor and the carrier: name the keys it would lose.
		if covered := anchorAdoptionCovers(before.local, before, next); errors.As(covered, &typed) {
			return typed
		}
	}
	return &AnchorRefusal{Reason: reason}
}

func anchorRefusalReason(before, next *anchorCursor) AnchorRefusalReason {
	if before == nil || next == nil || before.awaiting || next.awaiting {
		return AnchorRefusedNotSuccessor
	}
	old, other := before.owner.Inspect(), next.owner.Inspect()
	switch {
	case old.ClusterID != other.ClusterID || old.HistoryID != other.HistoryID || !old.InitializedAt.Equal(other.InitializedAt):
		return AnchorRefusedHistory
	case old.Phase == accepted.Managed && other.Phase != accepted.Managed:
		return AnchorRefusedDormantOwner
	case old.Phase == accepted.Managed && (before.mark.DomainID != next.mark.DomainID || old.PolicyID != other.PolicyID || old.SeedDigest != other.SeedDigest):
		return AnchorRefusedPolicy
	}
	if old.Phase == accepted.Managed && other.Phase == accepted.Managed {
		oldState, err := before.owner.State()
		if err != nil {
			return AnchorRefusedNotSuccessor
		}
		newState, err := next.owner.State()
		if err != nil {
			return AnchorRefusedNotSuccessor
		}
		if !newState.RetainsRecoveryHistory(oldState) {
			return AnchorRefusedRecoveryHistory
		}
	}
	return AnchorRefusedNotSuccessor
}

// AnchorLocalView is a detached copy of this member's node-local root-anchor
// carrier for diagnostics. It is not a serving credential, receipt or grant.
type AnchorLocalView struct {
	// Phase is "awaiting" before the first managed owner, otherwise "managed".
	Phase string
	// Owner is the accepted owner the carrier is bound to.
	Owner accepted.Version
	// RetiredThrough is the highest owner generation whose output is retired.
	RetiredThrough uint64
	// Denied lists every key this member denied or carried, by identity.
	Denied []AnchorLocalDenial
	// Terminal means the carrier can never publish again.
	Terminal bool
}

// AnchorLocalDenial is one denied key and the owner of its first receipt.
type AnchorLocalDenial struct {
	ID         string
	FirstOwner accepted.Version
}

// ReadAnchorStatus reads the installed image and this member's node-local
// carrier in one read-only transaction, for diagnostics only. It grants no
// publication, command or anchor operation. A database without a carrier
// (before schema 8) returns a nil view.
func (s *Store) ReadAnchorStatus(ctx context.Context) (*replication.Image, *AnchorLocalView, error) {
	if s.isClosed() {
		return nil, nil, store.ErrClosed
	}
	tx, err := s.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = tx.Rollback() }()
	image, err := s.readGeneration(ctx, tx)
	if err != nil {
		return nil, nil, err
	}
	local, err := loadAnchorLocal(ctx, tx)
	if err != nil || local == nil {
		return image, nil, err
	}
	r := local.record
	view := &AnchorLocalView{Phase: r.Phase, Owner: r.Owner, RetiredThrough: r.RetiredThrough, Terminal: r.Terminal}
	for _, d := range r.Denied {
		view.Denied = append(view.Denied, AnchorLocalDenial{ID: d.ID, FirstOwner: d.FirstOwner})
	}
	slices.SortFunc(view.Denied, func(a, b AnchorLocalDenial) int {
		switch {
		case a.ID < b.ID:
			return -1
		case a.ID > b.ID:
			return 1
		}
		return 0
	})
	return image, view, nil
}
