package main

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// The primary acknowledges a forwarded write under the writer exclusion of
// that write's own commit. Other primary writers that take no publication
// gate, as the status reconciler does for an object that an earlier forwarded
// write just created, wait for the answer. Before, such a commit between a
// forwarded write's commit and its answer withheld the answer, and the
// follower reported `completion_unknown` for a write that had committed.
func TestRootMemberForwardIsAcknowledgedBesideOtherPrimaryCommits(t *testing.T) {
	f := newMemberForwardHTTPFixture(t, false)
	primary := f.primary.s.opts.Store
	writer, err := primary.WriterToken()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	stop, stopped := make(chan struct{}), make(chan error, 1)
	commits := 0
	go func() {
		for n := 0; ; n++ {
			select {
			case <-stop:
				stopped <- nil
				return
			default:
			}
			if err := primary.Command(sqlite.WithWriterToken(ctx, writer), writer.Term(), func(ctx context.Context, tx *sqlite.CommandTx) error {
				return tx.PutSection(ctx, "dedup", replication.Entry{Key: "beside-forward", Data: []byte(`{"n":` + strconv.Itoa(n) + `}`)})
			}); err != nil {
				stopped <- err
				return
			}
			commits++
			// Leave the writer free between commits so that a forward can
			// enter; the next commit then queues behind its transaction.
			time.Sleep(300 * time.Microsecond)
		}
	}()
	const forwards = 40
	var failed []string
	for n := range forwards {
		name := "beside-" + strconv.Itoa(n)
		got := f.exchange(ctx, http.MethodPut, "/clientgroups/default/"+name, memberForwardGroup(name, "192.0.2.95"))
		if got.err != nil {
			t.Fatal(got.err)
		}
		if got.status != http.StatusCreated {
			failed = append(failed, name+": "+strconv.Itoa(got.status)+" "+string(got.body))
		}
	}
	close(stop)
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatal("the other primary writer failed", err)
		}
	case <-ctx.Done():
		t.Fatal("the other primary writer did not join")
	}
	if commits < forwards {
		t.Fatal("the other primary writer did not contend with the forwards", commits)
	}
	// Every forwarded write committed either way; the difference is whether
	// the follower was told.
	for n := range forwards {
		name := "beside-" + strconv.Itoa(n)
		if err := primary.Get(t.Context(), store.Key{Kind: "ClientGroup", Namespace: "default", Name: name}, &public.ClientGroup{}); err != nil {
			t.Error("forwarded write missing on the primary", name, err)
		}
	}
	if len(failed) != 0 {
		t.Fatalf("%d of %d committed forwarded writes were not acknowledged beside %d other commits:\n%s", len(failed), forwards, commits, strings.Join(failed, "\n"))
	}
}
