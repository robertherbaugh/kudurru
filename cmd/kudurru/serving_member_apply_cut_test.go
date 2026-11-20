package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	clusterreplica "github.com/robertherbaugh/kudurru/internal/cluster/replica"
	"github.com/robertherbaugh/kudurru/internal/store"
)

func TestRootMemberFollowerApplyCut(test *testing.T) {
	const unknown = "completion_unknown"
	names := []string{"cut-first", "cut-second", "cut-third", "cut-fourth"}
	documents := []any{
		memberForwardGroup(names[0], "192.0.2.91"),
		memberForwardGroup(names[1], "192.0.2.92"),
		memberForwardGroup(names[2], "192.0.2.93"),
		memberForwardGroup(names[3], "192.0.2.94"),
	}
	for _, cutoff := range []string{"lifetime", "held-pull"} {
		test.Run(cutoff, func(test *testing.T) {
			var mu sync.Mutex
			var commands []clusterreplica.Command
			fixture := newMemberForwardHTTPFixture(test, false, func(fixture *memberForwardHTTPFixture) {
				fixture.memberCommandSeen = func(command clusterreplica.Command) {
					mu.Lock()
					defer mu.Unlock()
					commands = append(commands, command)
				}
				if cutoff == "lifetime" {
					fixture.configureFollower = cutApplyAtLifetime
				} else {
					cutApplyAtHeldPull(test, fixture, names[0])
				}
			})
			primary := fixture.primary.s.opts.Store
			authority := primary.Authority()
			term := fixture.follower.opts.Store.Authority().Term
			if term != authority.Term || term.PrimaryID != authority.MemberID || term.PrimaryName == "" {
				test.Fatalf("follower term %+v does not identify primary %+v", term, authority.Term)
			}

			ctx, cancel := context.WithTimeout(test.Context(), 20*time.Second)
			defer cancel()
			answer := fixture.exchange(ctx, http.MethodPost, "/apply", documents)
			var applied api.List[api.ApplyResult]
			if answer.err != nil || answer.status != http.StatusOK || json.Unmarshal(answer.body, &applied) != nil || len(applied.Items) != len(names) {
				test.Fatalf("apply: status=%d err=%v body=%s", answer.status, answer.err, answer.body)
			}
			wantKeys := []string{"", api.KeyUnavailable, api.KeyUnavailable, api.KeyUnavailable}
			wantSent := names[:1]
			if cutoff == "lifetime" {
				wantKeys[1] = unknown
				wantSent = names[:2]
			}
			for index, item := range applied.Items {
				name, wantKey := names[index], wantKeys[index]
				if item.Name != name || item.Namespace != "default" || item.Kind != "ClientGroup" || item.ErrorKey != wantKey {
					test.Errorf("document %d: want ClientGroup default/%s with error key %q, got %+v", index, name, wantKey, item)
				}
				var persisted public.ClientGroup
				err := primary.Get(ctx, store.Key{Kind: "ClientGroup", Namespace: "default", Name: name}, &persisted)
				if wantKey == "" {
					if item.Result != api.ResultCreated || item.Error != "" || item.ResourceVersion == "" || err != nil || persisted.ResourceVersion != item.ResourceVersion {
						test.Errorf("%s: want created with the primary's durable resource version, got %+v; stored version=%q err=%v", name, item, persisted.ResourceVersion, err)
					}
					continue
				}
				if item.Result != api.ResultError || item.ResourceVersion != "" {
					test.Errorf("%s: want error without a resource version, got %+v", name, item)
				}
				var messageParts []string
				switch {
				case wantKey == unknown:
					if err != nil || persisted.ResourceVersion == "" {
						test.Errorf("%s: lost receipt must leave a durable write on the primary: version=%q err=%v", name, persisted.ResourceVersion, err)
					}
					messageParts = []string{"inspect the object", "do not replay", "the items after it were not applied", "send them to the enrolled primary " + term.PrimaryName}
				case cutoff == "held-pull":
					messageParts = []string{"the follower's source changed while this request ran (it installed newer data from the primary)",
						"this item was not sent and was not applied, and neither was any later one", "to the enrolled primary " + term.PrimaryName + ", which applies them in one request"}
				default:
					messageParts = []string{"the request's 30-second forwarding lifetime ended before this write was sent; it was not applied"}
				}
				if wantKey == api.KeyUnavailable && !errors.Is(err, store.ErrNotFound) {
					test.Errorf("%s: want unavailable item absent on the primary, got %v", name, err)
				}
				for _, part := range messageParts {
					if !strings.Contains(item.Error, part) {
						test.Errorf("%s: refusal %q does not contain %q", name, item.Error, part)
					}
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if len(commands) != len(wantSent) {
				test.Fatalf("primary received %d commands, want exactly %d", len(commands), len(wantSent))
			}
			for index, command := range commands {
				var input api.ForwardedObject
				if err := json.Unmarshal(command.Payload, &input); err != nil {
					test.Fatalf("primary command %d: decode object request: %v", index, err)
				}
				if command.Operation != memberObjectWrite || input.Name != wantSent[index] || command.ExpectedTerm != term {
					test.Errorf("primary command %d: want one write for %s on %+v, got %s for %s on %+v", index, wantSent[index], term, command.Operation, input.Name, command.ExpectedTerm)
				}
				if !command.NotAfter.Equal(commands[0].NotAfter) {
					test.Errorf("primary command %d did not reuse the first command's request deadline", index)
				}
			}
		})
	}
}

func cutApplyAtLifetime(follower *servingRuntime) {
	follower.memberForwardBound = 5 * time.Second
	relays := 0
	follower.memberRelay = func(scope context.Context) {
		relays++
		if relays == 2 {
			<-scope.Done()
		}
	}
}

func cutApplyAtHeldPull(test *testing.T, fixture *memberForwardHTTPFixture, first string) {
	test.Helper()
	const maxPullHold = time.Second
	startPull := make(chan struct{})
	var startOnce sync.Once
	fixture.scheduledPull = true
	fixture.configureFollower = func(follower *servingRuntime) {
		follower.memberPullStart, follower.memberMaxPullHold = startPull, maxPullHold
	}
	fixture.wrapObjectForward = func(forward api.ObjectForward) api.ObjectForward {
		return func(ctx context.Context, input api.ForwardedObject) (api.ForwardedObjectResult, bool, error) {
			result, handled, err := forward(ctx, input)
			if input.Name == first && handled && err == nil && result.Result == api.ResultCreated {
				startOnce.Do(func() { awaitCutApplyPull(ctx, test, fixture.follower, first, maxPullHold, startPull) })
			}
			return result, handled, err
		}
	}
}

func awaitCutApplyPull(ctx context.Context, test *testing.T, follower *servingRuntime, name string, maxHold time.Duration, startPull chan struct{}) {
	test.Helper()
	mirror := follower.opts.Store
	before := mirror.Authority().InstallSequence
	key := store.Key{Kind: "ClientGroup", Namespace: "default", Name: name}
	if err := mirror.Get(ctx, key, &public.ClientGroup{}); !errors.Is(err, store.ErrNotFound) {
		test.Errorf("before the scheduled pull started, follower should not have %s: %v", name, err)
	}
	started := time.Now()
	close(startPull)
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if mirror.Authority().InstallSequence > before && mirror.Get(ctx, key, &public.ClientGroup{}) == nil {
			if took := time.Since(started); took < maxHold {
				test.Errorf("scheduled pull installed %s after %s, before the %s hold expired", name, took, maxHold)
			}
			return
		}
		select {
		case <-ctx.Done():
			test.Errorf("scheduled pull did not install %s while apply was held between items: %v", name, ctx.Err())
			return
		case <-ticker.C:
		}
	}
}
