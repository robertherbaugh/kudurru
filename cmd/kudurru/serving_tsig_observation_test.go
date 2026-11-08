package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/tsigkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/servingbuild"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/tsigkeys"
)

// These are actual existing API routes with one tested command per fixture.
// Receipt/audit persistence is separate from any claim about ticker causation.
func TestServingTSIGCommittedWithoutAckDurableAudit(t *testing.T) {
	for _, operation := range []api.TSIGKeyOperation{api.TSIGKeyCreate, api.TSIGKeyReplace, api.TSIGKeyRevoke, api.TSIGKeyReseal} {
		t.Run(string(operation), func(t *testing.T) {
			f := newTSIGCommandFixture(t)
			server := tsigCommandHTTP(t, f.servingCommandFixture, nil)
			_ = tsigReadReceipt(t, tsigRouteRequest(t.Context(), t, server, f.token, http.MethodPost, "/tsigkeys", tsigCreateBody("unrelated-audit.", 0x73)), http.StatusCreated)
			var original api.TSIGKeyCommandReceipt
			if operation != api.TSIGKeyCreate {
				original = tsigReadReceipt(t, tsigRouteRequest(t.Context(), t, server, f.token, http.MethodPost, "/tsigkeys", tsigCreateBody("audit.", 0x74)), http.StatusCreated)
			}
			cut := f.s.opts.AuthKeys.owner.Capture()
			if !cut.Current() || cut.Value() == nil {
				t.Fatal("original audit encryption owner unavailable")
			}
			sealer, err := tsigkeys.NewSealer(cut.Value().current, cut.Value().previous)
			if err != nil {
				t.Fatal("prepare original audit encryption owner")
			}
			f.stopSource()
			ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
			defer cancel()
			before, err := f.s.opts.Store.ReadGeneration(ctx)
			if err != nil {
				t.Fatal("read original complete audit baseline")
			}
			auditBefore := tsigABQAudit(ctx, t, f.s.opts.Store, 0)
			if len(auditBefore) < 2 || auditBefore[len(auditBefore)-1].Actor == "" {
				t.Fatal("original route audit actor baseline unavailable")
			}
			cursor := auditBefore[len(auditBefore)-1].ID
			originalActor := auditBefore[len(auditBefore)-1].Actor
			path, body := "/tsigkeys", any(tsigCreateBody("audit.", 0x75))
			if operation != api.TSIGKeyCreate {
				path = "/tsigkeys/audit./" + string(operation)
				body = map[string]any{"uid": original.Key.UID, "resourceVersion": original.Key.ResourceVersion}
				if operation == api.TSIGKeyReplace {
					body = tsigReplaceBody(original.Key, 0x75)
				}
			}
			done := tsigABQStartRequest(ctx, t, server, f.token, path, body)
			joined := false
			t.Cleanup(func() {
				cancel()
				if !joined {
					timer := time.NewTimer(time.Second)
					defer timer.Stop()
					select {
					case <-done:
					case <-timer.C:
						t.Error("single original audit route did not join")
					}
				}
			})
			var committed *replication.Image
			for {
				changed := f.s.opts.Store.GenerationChanges()
				candidate, readErr := f.s.opts.Store.ReadGeneration(ctx)
				if readErr != nil {
					t.Fatal("read original audit command outcome")
				}
				if candidate.Revision > before.Revision {
					committed = candidate
					break
				}
				select {
				case <-changed:
				case <-ctx.Done():
					t.Fatal("single original audit command did not commit")
				}
			}
			if committed.Revision != before.Revision+1 || committed.ObjectResourceVersion != before.ObjectResourceVersion+1 || !bytes.Equal(tsigABQUnrelatedPayload(t, before, "audit."), tsigABQUnrelatedPayload(t, committed, "audit.")) {
				t.Fatal("original audit command changed more than its intended durable effect")
			}
			rowBefore := tsigABQImageRow(t, before, "audit.")
			row := tsigABQImageRow(t, committed, "audit.")
			if operation == api.TSIGKeyRevoke {
				if rowBefore == nil || row != nil {
					t.Fatal("committed revoke did not remove exactly its original row")
				}
			} else {
				if row == nil || row.Validate() != nil || row.ResourceVersion == original.Key.ResourceVersion {
					t.Fatal("committed audit command omitted its encrypted durable row")
				}
				material, previous, openErr := sealer.Open(row)
				want := byte(0x75)
				if operation == api.TSIGKeyReseal {
					want = 0x74
				}
				if openErr != nil || material == nil || previous || !bytes.Equal(material.Key().Secret, bytes.Repeat([]byte{want}, 32)) {
					t.Fatal("durable credential did not retain the original intended encrypted value")
				}
				binding, epoch := material.Binding()
				if binding.UID != string(row.UID) || binding.ClusterID != committed.Term.ClusterID || epoch != row.Spec.CredentialEpoch {
					t.Fatal("durable encrypted credential lost its original owner binding")
				}
				if rowBefore != nil && (row.UID != rowBefore.UID || row.CreationTimestamp != rowBefore.CreationTimestamp || row.Spec.Sealed == rowBefore.Spec.Sealed || (operation == api.TSIGKeyReseal) != (row.Spec.CredentialEpoch == rowBefore.Spec.CredentialEpoch)) {
					t.Fatal("replace or reseal changed the wrong credential incarnation")
				}
			}
			// Actual commit is established before ending only the original proof
			// lifetime. Source stays positively joined; there is no rescue request.
			f.offset.Store(int64(time.Hour))
			observation := time.NewTimer(time.Second)
			defer observation.Stop()
			var response *httptest.ResponseRecorder
			select {
			case response = <-done:
				joined = true
			case <-observation.C:
				t.Fatal("committed audit command renewed original credential lifetime")
			case <-ctx.Done():
				t.Fatal("committed audit command exceeded its original request bound")
			}
			receipt := tsigReadReceipt(t, response, http.StatusAccepted)
			if receipt.Operation != operation || receipt.Removed != (operation == api.TSIGKeyRevoke) {
				t.Fatal("missing acknowledgment changed the confirmed command intent")
			}
			if operation == api.TSIGKeyRevoke {
				if !reflect.DeepEqual(receipt.Key, original.Key) {
					t.Fatal("revoke receipt lost historical original metadata")
				}
			} else if receipt.Key.UID != string(row.UID) || receipt.Key.ResourceVersion != row.ResourceVersion || receipt.Key.KeyName != row.Spec.KeyName || receipt.Key.Algorithm != row.Spec.Algorithm || receipt.Key.Source != "store" || receipt.Key.CreatedAt == nil || !receipt.Key.CreatedAt.Equal(row.CreationTimestamp.Time.UTC().Truncate(time.Second)) || rowBefore != nil && receipt.Key.UID != original.Key.UID {
				t.Fatal("receipt did not identify its actual durable encrypted row")
			}
			after, err := f.s.opts.Store.ReadGeneration(ctx)
			if err != nil || !tsigABQSameImage(committed, after) {
				t.Fatal("missing acknowledgment rolled back or changed the confirmed effect")
			}
			newAudit := tsigABQAudit(ctx, t, f.s.opts.Store, cursor)
			allAudit := tsigABQAudit(ctx, t, f.s.opts.Store, 0)
			if len(newAudit) != 2 || len(allAudit) != len(auditBefore)+2 || !reflect.DeepEqual(allAudit[:len(auditBefore)], auditBefore) || !reflect.DeepEqual(allAudit[len(auditBefore):], newAudit) || f.s.opts.Store.VerifyAudit(ctx) != nil {
				t.Fatal("confirmed command omitted its exact safe lifecycle and automatic audit")
			}
			automatic := "tsigkey.update"
			switch operation {
			case api.TSIGKeyCreate:
				automatic = "tsigkey.create"
			case api.TSIGKeyRevoke:
				automatic = "tsigkey.delete"
			}
			for i, entry := range newAudit {
				wantAction, wantManager, wantRV := automatic, store.ManagerSystem, strconv.FormatUint(committed.ObjectResourceVersion, 10)
				if i == 1 {
					wantAction, wantManager, wantRV = "tsigkey."+string(operation), store.ManagerAPI, receipt.Key.ResourceVersion
				}
				if entry.ID <= cursor || entry.Actor != originalActor || entry.Key != (store.Key{Kind: kinds.KindTSIGKey, Name: kinds.ObjectName("audit.")}) || entry.Action != wantAction || entry.Manager != wantManager || entry.ResourceVersion != wantRV || entry.Result != sqlite.AuditSuccess || entry.Time.IsZero() || len(entry.Before) != 0 || len(entry.After) != 0 {
					t.Fatal("confirmed command audit exposed private documents or lost exact action bindings")
				}
			}
			encodedAudit, err := json.Marshal(newAudit)
			if err != nil {
				t.Fatal("encode safe audit for bounded disclosure check")
			}
			private := []string{sealer.Identity(), string(cut.Value().current), string(cut.Value().previous), base64.StdEncoding.EncodeToString(cut.Value().current), base64.StdEncoding.EncodeToString(cut.Value().previous)}
			for _, value := range []byte{0x73, 0x74, 0x75} {
				private = append(private, string(bytes.Repeat([]byte{value}, 32)), base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{value}, 32)))
			}
			for _, image := range []*replication.Image{before, committed} {
				input, sourceErr := tsigkeys.FromImage(ctx, image)
				if sourceErr != nil {
					t.Fatal("derive actual bounded disclosure source")
				}
				private = append(private, input.Identity())
				prepared, prepareErr := input.Prepare(sealer, f.source.baseline)
				if prepareErr != nil {
					t.Fatal("validate complete original encrypted disclosure source")
				}
				for _, name := range []string{"audit.", "unrelated-audit."} {
					if key := tsigABQImageRow(t, image, name); key != nil {
						identity, ok := prepared.Keys().Identity(name)
						if !ok {
							t.Fatal("original durable credential fingerprint unavailable")
						}
						private = append(private, key.Spec.Sealed, key.Spec.CredentialEpoch, identity.CredentialVersion, identity.ProviderVersion)
					}
				}
			}
			for _, document := range [][]byte{encodedAudit, response.Body.Bytes()} {
				for _, forbidden := range append(private, `"sealed"`, `"credentialEpoch"`, `"secret"`, `"fingerprint"`, `"encryptionIdentity"`, `"sourceIdentity"`) {
					if forbidden != "" && bytes.Contains(document, []byte(forbidden)) {
						t.Fatal("confirmed command audit or receipt disclosed private credential data")
					}
				}
			}
		})
	}
}

func tsigABQImageRow(t *testing.T, image *replication.Image, keyName string) *kinds.TSIGKey {
	t.Helper()
	var result *kinds.TSIGKey
	for _, raw := range image.Payload.Objects {
		var header struct{ Kind string }
		if json.Unmarshal(raw, &header) != nil {
			t.Fatal("decode complete source object header")
		}
		if header.Kind != kinds.KindTSIGKey {
			continue
		}
		var key kinds.TSIGKey
		if json.Unmarshal(raw, &key) != nil || key.Validate() != nil {
			t.Fatal("decode original encrypted source row")
		}
		if key.Spec.KeyName == keyName {
			if result != nil {
				t.Fatal("duplicate original encrypted source binding")
			}
			result = &key
		}
	}
	return result
}

func tsigABQUnrelatedPayload(t *testing.T, image *replication.Image, keyName string) []byte {
	t.Helper()
	payload := image.Payload
	payload.Objects = nil
	for _, raw := range image.Payload.Objects {
		var header struct {
			Kind     string
			Metadata struct{ Name string }
		}
		if json.Unmarshal(raw, &header) != nil {
			t.Fatal("decode unrelated complete object binding")
		}
		if header.Kind != kinds.KindTSIGKey || header.Metadata.Name != kinds.ObjectName(keyName) {
			payload.Objects = append(payload.Objects, raw)
		}
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal("encode unrelated complete payload comparison")
	}
	return encoded
}

// These call the unchanged production observer on a private receiver. They do
// not attribute quiet causality to the actual TSIGKeyCommand API route.
func TestServingTSIGObserverQuietOriginalExpiry(t *testing.T) {
	tsigABQObserver(t, "expiry")
}

func TestServingTSIGObserverOriginalCancellationAndDeadline(t *testing.T) {
	for _, mode := range []string{"cancellation", "deadline"} {
		t.Run(mode, func(t *testing.T) { tsigABQObserver(t, mode) })
	}
}

type tsigABQObserverOutcome struct {
	committed        bool
	commandErr       error
	observerErr      error
	failure          string
	image            *replication.Image
	publisherChanged <-chan struct{}
	authorityChanged <-chan struct{}
	windowOpen       bool
	pendingUnchanged bool
	writerEligible   bool
	contextAlive     bool
}

func tsigABQObserver(t *testing.T, mode string) {
	t.Helper()
	f := newTSIGCommandFixture(t)
	f.stopSource()
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	requestCtx := ctx
	if mode == "deadline" {
		var cancelEarlier context.CancelFunc
		// This is the original earlier request deadline, established BEFORE
		// authentication and commit. It is never renewed after publication.
		requestCtx, cancelEarlier = context.WithTimeout(ctx, time.Second)
		defer cancelEarlier()
	}
	var clockOffset, clockCalls, lastSample atomic.Int64
	var announced atomic.Bool
	firstSample := make(chan time.Time, 1)
	clock := func() time.Time {
		sampled := f.now.Add(time.Duration(clockOffset.Load()))
		lastSample.Store(sampled.UnixNano())
		clockCalls.Add(1)
		if announced.CompareAndSwap(false, true) {
			select {
			case firstSample <- sampled:
			default:
			}
		}
		return sampled
	}
	observer := &servingRuntime{opts: servingRuntimeOptions{Store: f.s.opts.Store, Now: clock}, publisher: f.s.publisher}
	done := make(chan tsigABQObserverOutcome, 1)
	joined := false
	t.Cleanup(func() {
		cancel()
		if !joined {
			timer := time.NewTimer(time.Second)
			defer timer.Stop()
			select {
			case <-done:
			case <-timer.C:
				t.Error("original production observer invocation did not join")
			}
		}
	})
	// This unmounted middleware callback establishes only Q's normal committed
	// target through existing private callbacks. It adds no route or fault hook
	// and is not a C/D/E/F case or an actual API TSIGKeyCommand invocation.
	var outcome tsigABQObserverOutcome
	called := false
	handler := f.http.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		called = true
		operationCtx, cancelOperation := context.WithTimeout(r.Context(), tsigKeyCommandBound)
		defer cancelOperation()
		raw, err := servinghttp.Credential(operationCtx)
		if err != nil {
			outcome.failure = "original observer raw proof unavailable"
			return
		}
		view, err := servinghttp.Component[*servingauth.View](operationCtx, "auth")
		if err != nil {
			outcome.failure = "original observer auth view unavailable"
			return
		}
		owner, err := servinghttp.Component[*servingbuild.RuntimeOwner](operationCtx, "runtime")
		if err != nil {
			outcome.failure = "original observer runtime owner unavailable"
			return
		}
		change := api.TSIGKeyChange{Operation: api.TSIGKeyCreate, KeyName: "observer.", Algorithm: "hmac-sha256", Secret: bytes.Repeat([]byte{0x76}, 32)}
		attrs, err := tsigKeyAttributes(change)
		if err != nil {
			outcome.failure = "original observer attributes unavailable"
			return
		}
		proof, err := servingauth.CaptureOperation(operationCtx, view, raw, attrs, f.s.opts.Now())
		if err != nil {
			outcome.failure = "original observer operation proof unavailable"
			return
		}
		plan, err := owner.PlanTSIGChange(tsigkeys.MutationInput{Operation: tsigkeys.Create, KeyName: change.KeyName, Algorithm: change.Algorithm, Secret: change.Secret})
		if err != nil {
			outcome.failure = "original observer intended target unavailable"
			return
		}
		originalWriter := f.s.primary.writer.active.Load()
		target := tsigKeyPublication{original: owner, proof: proof, raw: raw, attrs: attrs, dependency: view.DependencyIdentity(), encryption: owner.TSIGEncryptionIdentity(), writerCurrent: func() bool { return f.s.primary.writer.current(originalWriter) }}
		outcome.committed, outcome.commandErr = f.s.currentAuthCommandThen(operationCtx, func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies) error {
			principal, authErr := servingauth.ReauthorizeOperationCurrent(ctx, tx, deps, proof, f.s.opts.Now)
			if authErr != nil {
				return authErr
			}
			if originalErr := target.checkOriginal(ctx, f.s.opts.Now()); originalErr != nil {
				return originalErr
			}
			metadata, stageErr := plan.Stage(ctx, tx)
			if stageErr != nil {
				return stageErr
			}
			post, readErr := tsigkeys.ReadTx(ctx, tx)
			if readErr != nil {
				return readErr
			}
			target.source, target.principal = post.Identity(), tsigPrincipalBinding(principal)
			if auditErr := tx.RecordAudit(ctx, sqlite.AuditEntry{Action: "tsigkey.create", Key: store.Key{Kind: kinds.KindTSIGKey, Name: attrs.Name}, ResourceVersion: metadata.ResourceVersion, Manager: store.ManagerAPI, Reason: "stored TSIG lifecycle command"}); auditErr != nil {
				return auditErr
			}
			return tx.RequireCommitCheck(func(ctx context.Context) error { return target.checkOriginal(ctx, f.s.opts.Now()) })
		}, func(ctx context.Context, writer serving.TermActivation) error {
			f.s.publisher.Wake()
			image, readErr := f.s.opts.Store.ReadGeneration(ctx)
			if readErr != nil {
				outcome.failure = "actual committed observer image unavailable"
				return serving.ErrComponents
			}
			post, sourceErr := tsigkeys.FromImage(ctx, image)
			if sourceErr != nil || post.Identity() != target.source {
				outcome.failure = "observer target differs from the original same-transaction postimage"
				return serving.ErrComponents
			}
			outcome.image = image
			for {
				changed, authority := f.s.publisher.Subscribe(), f.s.publisher.AuthorityChanges()
				status := f.s.publisher.Status()
				// A completed preparation of this actual committed installation is
				// the bounded read-only prerequisite. No notified window is retried
				// after the production observer or its first clock sample starts.
				if !status.Preparing && status.InstalledComplete && status.Installed.Same(image.Generation()) {
					lease, captureErr := f.s.publisher.CaptureHTTP(ctx)
					if lease != nil {
						lease.Release()
					}
					if !errors.Is(captureErr, serving.ErrPending) || status.ProtectedReady || f.s.writerCurrent(ctx, writer) != nil || target.checkOriginal(ctx, f.s.opts.Now()) != nil || !tsigABQChannelOpen(changed) || !tsigABQChannelOpen(authority) {
						outcome.failure = "original pending observer prerequisite failed"
						return serving.ErrComponents
					}
					outcome.publisherChanged, outcome.authorityChanged = changed, authority
					outcome.observerErr = observer.awaitTSIGKeyPublication(ctx, writer, target)
					after := f.s.publisher.Status()
					outcome.windowOpen = tsigABQChannelOpen(changed) && tsigABQChannelOpen(authority)
					outcome.pendingUnchanged = !after.ProtectedReady && after.InstalledComplete && after.Installed.Same(image.Generation()) && reflect.DeepEqual(after.Served, status.Served)
					outcome.writerEligible = f.s.primary.writer.current(originalWriter)
					outcome.contextAlive = ctx.Err() == nil
					return outcome.observerErr
				}
				select {
				case <-ctx.Done():
					outcome.failure = "actual observer prerequisite exceeded original context"
					return ctx.Err()
				case <-changed:
				case <-authority:
				}
			}
		})
	}))
	request := httptest.NewRequest(http.MethodPost, "http://kudurru.test/api/v1/tsigkeys", nil).WithContext(requestCtx)
	request.Header.Set("Authorization", "Bearer "+f.token)
	go func() {
		handler.ServeHTTP(httptest.NewRecorder(), request)
		if !called {
			outcome.failure = "actual original middleware refused observer setup"
		}
		done <- outcome // complete middleware/lease cleanup precedes positive join
	}()
	select {
	case sampled := <-firstSample:
		if !sampled.Equal(f.now) {
			t.Fatal("first production observer sample was not originally valid")
		}
	case early := <-done:
		joined = true
		if early.failure != "" {
			t.Fatal(early.failure)
		}
		t.Fatal("production observer ended before its original valid sample")
	case <-ctx.Done():
		t.Fatal("production observer did not enter within the original bound")
	}
	want := error(servingauth.ErrCredentials)
	switch mode {
	case "expiry":
		clockOffset.Store(int64(time.Hour))
	case "cancellation":
		want = context.Canceled
		cancel()
	default:
		want = context.DeadlineExceeded
	}
	observation := time.NewTimer(time.Second)
	defer observation.Stop()
	var result tsigABQObserverOutcome
	select {
	case result = <-done:
		joined = true
	case <-observation.C:
		t.Fatal("original observer did not join within its unchanged observation budget")
	}
	if result.failure != "" {
		t.Fatal(result.failure)
	}
	if !result.committed || !errors.Is(result.observerErr, want) || !errors.Is(result.commandErr, want) || !result.windowOpen || !tsigABQChannelOpen(result.publisherChanged) || !tsigABQChannelOpen(result.authorityChanged) || !result.pendingUnchanged || !result.writerEligible {
		t.Fatal("original observer outcome or unnotified pending-window premises failed")
	}
	if mode == "expiry" && (!result.contextAlive || requestCtx.Err() != nil || clockCalls.Load() < 2 || lastSample.Load() != f.now.Add(time.Hour).UnixNano()) {
		t.Fatal("quiet expiry lacked a later invalid sample under the still-live original context")
	}
	if mode != "expiry" && (result.contextAlive || !errors.Is(requestCtx.Err(), want)) {
		t.Fatal("observer did not retain the original cancellation or earlier deadline")
	}
	// Independent post-completion verification is not further observer work and
	// grants no renewed mutation/response lifetime after a canceled request.
	after, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil || !tsigABQSameImage(result.image, after) || f.s.opts.Store.VerifyAudit(t.Context()) != nil {
		t.Fatal("original observer lifetime failure changed the actual committed target")
	}
}

func tsigABQChannelOpen(ch <-chan struct{}) bool {
	if ch == nil {
		return false
	}
	select {
	case <-ch:
		return false
	default:
		return true
	}
}
