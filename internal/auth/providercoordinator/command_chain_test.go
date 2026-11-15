package providercoordinator

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func chainPut(key string) CurrentCommandFunc {
	return func(ctx context.Context, tx *sqlite.CommandTx, _ servingauth.Dependencies) error {
		return tx.PutSection(ctx, "dedup", replication.Entry{Key: key, Data: []byte(`{}`)})
	}
}

func chainNoop(context.Context, *sqlite.CommandTx, servingauth.Dependencies) error { return nil }

func chainHas(t *testing.T, s *sqlite.Store, key string) bool {
	t.Helper()
	found := false
	command(t, s, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := tx.GetSection(ctx, "dedup", key)
		found = err == nil
		return nil
	})
	return found
}

// A chain follows its own commits and the node's ungated follow-up writes. The
// first state-changing command of another caller through the coordinator ends
// it for good: the refused commands never run, and a later command of the same
// chain is refused again instead of adopting the newer state.
func TestChainFollowsOwnReceiptsAndRefusesAForeignCommand(t *testing.T) {
	f := newFixture(t)
	ctx, target := writer(t, f.store)
	chain := f.coordinator.NewChain()
	own := WithChain(ctx, chain)
	if _, ok := chain.Receipt(); ok || chain.Anchored() {
		t.Fatal("a new chain claimed an anchor")
	}
	if err := f.coordinator.WithCommand(own, target, chainPut("own-1")); err != nil {
		t.Fatal(err)
	}
	receipt, ok := chain.Receipt()
	if !ok || !chain.Anchored() || !reflect.DeepEqual(receipt, f.store.Authority()) {
		t.Fatal("the chain's first commit did not anchor it on the Store's own receipt")
	}
	// A writer that takes only the Store lock, as status reconciliation does,
	// advances the generation without moving the chain's anchor.
	command(t, f.store, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return tx.PutSection(ctx, "dedup", replication.Entry{Key: "follow-up", Data: []byte(`{}`)})
	})
	if reflect.DeepEqual(receipt, f.store.Authority()) {
		t.Fatal("the follow-up writer did not advance the generation")
	}
	// Commands of another caller that change nothing, or that are refused and
	// rolled back, are not foreign commits either.
	if err := f.coordinator.WithCommand(ctx, target, chainNoop); err != nil {
		t.Fatal(err)
	}
	refusal := errors.New("fixture refusal")
	if err := f.coordinator.WithCommand(ctx, target, func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies) error {
		if err := chainPut("refused")(ctx, tx, deps); err != nil {
			return err
		}
		return refusal
	}); !errors.Is(err, refusal) {
		t.Fatal(err)
	}
	if err := f.coordinator.WithCommand(own, target, chainPut("own-2")); err != nil {
		t.Fatal("the chain did not follow its own receipt across follow-up writes", err)
	}
	second, ok := chain.Receipt()
	if !ok || !reflect.DeepEqual(second, f.store.Authority()) || second.Installed.Revision <= receipt.Installed.Revision {
		t.Fatal("the chain did not move to its second commit's receipt")
	}
	// An own no-op and an own clean refusal keep the anchor.
	if err := f.coordinator.WithCommand(own, target, chainNoop); err != nil {
		t.Fatal(err)
	}
	if err := f.coordinator.WithCommand(own, target, func(context.Context, *sqlite.CommandTx, servingauth.Dependencies) error { return refusal }); !errors.Is(err, refusal) {
		t.Fatal(err)
	}
	if again, ok := chain.Receipt(); !ok || !reflect.DeepEqual(again, second) {
		t.Fatal("an own no-op or refusal moved the anchor")
	}

	if err := f.coordinator.WithCommand(ctx, target, chainPut("foreign")); err != nil {
		t.Fatal(err)
	}
	for attempt := range 2 {
		called := false
		before := f.store.Authority()
		err := f.coordinator.WithCommand(own, target, func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies) error {
			called = true
			return chainPut("own-3")(ctx, tx, deps)
		})
		if !errors.Is(err, ErrForeign) || called || !reflect.DeepEqual(before, f.store.Authority()) {
			t.Fatal("a chained command ran after a foreign commit", attempt, err, called)
		}
	}
	if chainHas(t, f.store, "own-3") || !chainHas(t, f.store, "own-2") || !chainHas(t, f.store, "foreign") {
		t.Fatal("the refused item was applied, or an earlier commit was lost")
	}
	if again, ok := chain.Receipt(); !ok || !reflect.DeepEqual(again, second) {
		t.Fatal("a refused chain adopted the foreign commit")
	}
}

// A chain whose first completed command changed nothing is anchored on the
// state its source already contained. A rolled-back first command does not
// anchor it, so root keeps requiring the original source for the next one.
func TestChainAnchorsOnItsFirstCompletedCommand(t *testing.T) {
	f := newFixture(t)
	ctx, target := writer(t, f.store)
	if err := f.coordinator.WithCommand(ctx, target, chainPut("earlier")); err != nil {
		t.Fatal(err)
	}
	chain := f.coordinator.NewChain()
	own := WithChain(ctx, chain)
	refusal := errors.New("fixture refusal")
	if err := f.coordinator.WithCommand(own, target, func(context.Context, *sqlite.CommandTx, servingauth.Dependencies) error { return refusal }); !errors.Is(err, refusal) || chain.Anchored() {
		t.Fatal("a refused first command anchored the chain", err)
	}
	if err := f.coordinator.WithCommand(own, target, chainNoop); err != nil || !chain.Anchored() {
		t.Fatal("a completed first command did not anchor the chain", err)
	}
	if _, ok := chain.Receipt(); ok {
		t.Fatal("a chain without an own commit claimed a commit receipt")
	}
	command(t, f.store, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return tx.PutSection(ctx, "dedup", replication.Entry{Key: "follow-up", Data: []byte(`{}`)})
	})
	if err := f.coordinator.WithCommand(own, target, chainNoop); err != nil {
		t.Fatal("a follow-up write ended a chain that had not committed", err)
	}
	if err := f.coordinator.WithCommand(ctx, target, chainPut("foreign")); err != nil {
		t.Fatal(err)
	}
	if err := f.coordinator.WithCommand(own, target, chainPut("own")); !errors.Is(err, ErrForeign) || chainHas(t, f.store, "own") {
		t.Fatal("a chain anchored on a no-op continued after a foreign commit", err)
	}
}

// A chain belongs to one coordinator and one request, one command at a time.
func TestChainRefusesAnotherCoordinatorAndReentry(t *testing.T) {
	f, other := newFixture(t), newFixture(t)
	ctx, target := writer(t, f.store)
	if err := f.coordinator.WithCommand(WithChain(ctx, other.coordinator.NewChain()), target, chainPut("stolen")); !errors.Is(err, ErrInvalid) || chainHas(t, f.store, "stolen") {
		t.Fatal("a chain of another coordinator was admitted", err)
	}
	chain := f.coordinator.NewChain()
	chain.busy.Store(true)
	if err := f.coordinator.WithCommand(WithChain(ctx, chain), target, chainPut("reentered")); !errors.Is(err, ErrInvalid) || chainHas(t, f.store, "reentered") {
		t.Fatal("a chain ran two commands at once", err)
	}
	chain.busy.Store(false)
	if err := f.coordinator.WithCommand(WithChain(ctx, chain), target, chainPut("admitted")); err != nil || chain.busy.Load() {
		t.Fatal("a released chain was refused or stayed busy", err)
	}
	// A refusal leaves the chain usable for the request's next item.
	if err := f.coordinator.WithCommand(ctx, target, chainPut("foreign")); err != nil {
		t.Fatal(err)
	}
	if err := f.coordinator.WithCommand(WithChain(ctx, chain), target, chainNoop); !errors.Is(err, ErrForeign) || chain.busy.Load() {
		t.Fatal("a refused chain stayed busy or was admitted", err)
	}
}

// Every command path of the coordinator moves the mark: a command with its own
// output and a member command forwarded from a follower are foreign commits
// for a chained request, also when their output fails after the commit.
func TestChainTreatsOutputAndMemberCommandsAsForeign(t *testing.T) {
	transport := errors.New("transport failed")
	for _, mode := range []string{"output", "member"} {
		t.Run(mode, func(t *testing.T) {
			f := newMemberOutputFixture(t)
			ctx, target := writer(t, f.store)
			chain := f.coordinator.NewChain()
			own := WithChain(ctx, chain)
			if err := f.coordinator.WithCommand(own, target, chainPut("own-1")); err != nil {
				t.Fatal(err)
			}
			var committed bool
			var err error
			if mode == "output" {
				finite, cancel := context.WithTimeout(ctx, 5*time.Second)
				defer cancel()
				_, token, readErr := f.store.ReadServingCandidate(finite)
				if readErr != nil {
					t.Fatal(readErr)
				}
				request, captureErr := f.store.CaptureCommandOutput(finite, token)
				if captureErr != nil {
					t.Fatal(captureErr)
				}
				committed, err = f.coordinator.WithCommandOutput(finite, target, request, chainPut("foreign"), func(context.Context) error { return transport })
			} else {
				memberCtx, memberTarget, request := f.request(t)
				if memberTarget != (serving.TermActivation{Term: target.Term, Incarnation: target.Incarnation}) {
					t.Fatal("fixture writer changed")
				}
				committed, err = f.coordinator.WithMemberCommandOutput(memberCtx, memberTarget, request, chainPut("foreign"), func(context.Context) error { return transport })
			}
			if !committed || !errors.Is(err, transport) || !chainHas(t, f.store, "foreign") {
				t.Fatal("the foreign command did not commit with a failed output", committed, err)
			}
			if err := f.coordinator.WithCommand(own, target, chainPut("own-2")); !errors.Is(err, ErrForeign) || chainHas(t, f.store, "own-2") {
				t.Fatal("a chained command followed a foreign commit whose output failed", err)
			}
		})
	}
}
