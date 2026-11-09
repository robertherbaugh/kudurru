package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/providerresources"
	clusterreplica "github.com/robertherbaugh/kudurru/internal/cluster/replica"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/servingbuild"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// forwardMemberBlocking runs inside the original HTTP lease but before body
// decoding. Other domains retain their existing closed command paths. The
// follower reports only the primary's authenticated commit, never local serving
// convergence. Static nodes and primary/standalone commands keep their route.
func (h *servingHTTP) forwardMemberBlocking(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != h.opts.RoutePrefix+"/dns/blocking" {
			next.ServeHTTP(w, r)
			return
		}
		token, err := servinghttp.SourceToken(r.Context())
		if err != nil {
			servingHTTPError(w, r, err)
			return
		}
		if token.Authority().Mode != replication.Follower {
			next.ServeHTTP(w, r)
			return
		}
		started := false
		if err := h.forwardBlocking(w, r, token, &started); err != nil {
			if started {
				panic(http.ErrAbortHandler)
			}
			servingHTTPError(w, r, err)
		}
	})
}

func (h *servingHTTP) forwardBlocking(w http.ResponseWriter, r *http.Request, token sqlite.GenerationToken, started *bool) error {
	s := h.opts.Runtime
	if s.memberForward == nil || s.opts.Config.Cluster.Role == "static" {
		return serving.ErrPending
	}
	view, _, err := h.permission(r.Context(), "blocking", "toggle")
	if err != nil {
		return err
	}
	raw, err := servinghttp.Credential(r.Context())
	if err != nil {
		return err
	}
	if err := h.factorCSRF(r, view, raw); err != nil {
		return err
	}
	if r.URL.RawQuery != "" {
		return servingHTTPInvalid("blocking command does not accept query parameters")
	}
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		return servingHTTPInvalid("blocking command requires application/json")
	}
	request, err := s.memberForward.Capture(r.Context(), token)
	if err != nil {
		return err
	}
	issued := s.opts.Now().UTC()
	commandID := uuid.NewString()
	bounded := &clusterBoundedWriter{ResponseWriter: w, ctx: r.Context(), deadline: request.Deadline()}
	if err := bounded.SetReadDeadline(request.Deadline()); err != nil {
		return err
	}
	if err := bounded.SetWriteDeadline(request.Deadline()); err != nil {
		return err
	}
	done := make(chan struct{})
	stop := context.AfterFunc(r.Context(), func() { _ = bounded.SetReadDeadline(time.Now()); close(done) })
	defer func() {
		if !stop() {
			<-done
		}
	}()
	body, err := io.ReadAll(http.MaxBytesReader(bounded, r.Body, 4<<10))
	if err != nil {
		return servingHTTPInvalid("invalid blocking command body")
	}
	var in struct {
		Blocking *bool      `json:"blocking"`
		Timer    *float64   `json:"timer,omitempty"`
		Until    *time.Time `json:"until,omitempty"`
	}
	if err := decodeReviewedCommandJSON(body, &in, "invalid blocking command body"); err != nil {
		return err
	}
	if in.Blocking == nil {
		return servingHTTPInvalid("blocking is required")
	}
	change := api.BlockingChange{Blocking: *in.Blocking}
	if in.Timer != nil {
		change.HasTimer, change.TimerSeconds = true, *in.Timer
	}
	if in.Until != nil {
		change.HasUntil, change.Until = true, *in.Until
	}
	// Resolve relative intent against the original pre-body observation once.
	status, err := blockingStatus(change, issued)
	if err != nil {
		return err
	}
	wire := memberBlockingChange{Blocking: in.Blocking}
	if status.DisabledUntil != nil {
		until := status.DisabledUntil.UTC()
		wire.Until = &until
	}
	payload, err := json.Marshal(wire)
	if err != nil {
		return err
	}
	return s.withMemberForward(r.Context(), view, raw, blockingToggleAttrs, func(ctx context.Context, principal auth.Principal, lifetime *servingauth.CredentialLifetime, current func(context.Context) error) error {
		proof, err := clusterreplica.NewCredentialProof(raw)
		if err != nil {
			return err
		}
		reference, err := proof.Reference()
		if err != nil {
			return err
		}
		settings, err := view.Settings()
		if err != nil {
			return err
		}
		kind := map[servingauth.CredentialKind]string{servingauth.APIToken: "api-token", servingauth.Cookie: "session", servingauth.OIDCJWT: "oidc"}[raw.Kind]
		deadline, _ := ctx.Deadline()
		command := clusterreplica.Command{Format: clusterreplica.CommandFormat, Version: 2, ID: commandID,
			ExpectedTerm: request.OriginalTerm(), Hops: 1, IssuedAt: issued, NotAfter: deadline, Operation: memberBlockingToggle,
			Subject:    clusterreplica.SubjectRef{Kind: principal.Kind, ID: principal.ID, Provider: principal.Provider, CredentialKind: kind, CredentialID: reference, IssuedAt: principal.CredentialIssuedAt},
			Credential: proof, Preconditions: []clusterreplica.Precondition{{Kind: "Kudurru", Name: "default", ResourceVersion: settings.ResourceVersion}}, Payload: payload}
		_, err = s.memberForward.Forward(ctx, request, command, func(scope context.Context, out clusterreplica.Outcome) error {
			var receipt blockingReceipt
			if out.Status != http.StatusOK || decodeReviewedCommandJSON(out.Body, &receipt, "invalid primary command receipt") != nil ||
				!receipt.Committed || receipt.Publication != "pending" || receipt.SettingsUID != string(settings.UID) || receipt.ResourceVersion == "" ||
				receipt.Disabled != status.Disabled || (receipt.DisabledUntil == nil) != (status.DisabledUntil == nil) {
				return clusterreplica.ErrProtocol
			}
			if status.DisabledUntil != nil && !receipt.DisabledUntil.Equal(status.DisabledUntil.Time) {
				return clusterreplica.ErrProtocol
			}
			encoded, err := json.Marshal(receipt)
			if err != nil {
				return err
			}
			return lifetime.WithOutput(scope, func(output context.Context) error {
				return emitForwardedBlocking(output, bounded, request, encoded, started, current)
			})
		})
		if err != nil {
			return errors.Join(clusterreplica.ErrUnavailable, err)
		}
		return nil
	})
}

// The real order is publication -> resources -> host/auth keys -> DNS keys ->
// catalog -> transferred owners -> Store. All remain excluded through both the
// member POST and public response flush. No writer gate or SQL transaction is
// held while acquiring the remote response, and no replacement lease is captured.
func (s *servingRuntime) withMemberForward(ctx context.Context, view *servingauth.View, proof servingauth.Credential, attrs rbac.Attributes, fn func(context.Context, auth.Principal, *servingauth.CredentialLifetime, func(context.Context) error) error) error {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	stop := context.AfterFunc(s.life, func() { cancel(); close(done) })
	defer func() {
		if !stop() {
			<-done
		}
		cancel()
	}()
	lease, err := servinghttp.CloneLease(ctx)
	if err != nil {
		return err
	}
	defer lease.Release()
	runtime, err := servinghttp.Component[*servingbuild.RuntimeOwner](ctx, "runtime")
	if err != nil {
		return err
	}
	_, release, err := s.publisher.Pin(ctx)
	if err != nil {
		return err
	}
	defer release()
	return s.resources.WithCurrent(ctx, func(resources *providerresources.Pinned) error {
		deadline, err := resources.Deadline()
		if err != nil {
			return err
		}
		if !deadline.IsZero() {
			var cancel context.CancelFunc
			ctx, cancel = context.WithDeadline(ctx, deadline)
			defer cancel()
		}
		return s.host.PinHostBounded(ctx, func(ctx context.Context, hostCurrent func() bool) error {
			return s.opts.Keyrings.WithCurrent(ctx, func(keys servingbuild.KeyringCut) error {
				return s.catalog.WithCurrent(ctx, func() error {
					return s.opts.Transferred.WithCurrent(ctx, func() error {
						check := func(scope context.Context) error {
							if err := s.life.Err(); err != nil {
								return err
							}
							if !hostCurrent() || !keys.Current() || !runtime.DependenciesCurrent() {
								return serving.ErrDependency
							}
							if err := resources.CurrentAt(s.opts.Now()); err != nil {
								return err
							}
							return s.publisher.CheckReadLease(scope, lease)
						}
						if err := check(ctx); err != nil {
							return err
						}
						principal, _, lifetime, err := view.AuthorizeOriginalTransition(ctx, proof, attrs, s.opts.Now)
						if err != nil {
							return err
						}
						current := func(scope context.Context) error {
							if err := lifetime.Check(scope); err != nil {
								return err
							}
							return check(scope)
						}
						if err := lifetime.WithOutput(ctx, func(bounded context.Context) error { return fn(bounded, principal, lifetime, current) }); err != nil {
							return err
						}
						return current(ctx)
					})
				})
			})
		})
	})
}

func emitForwardedBlocking(ctx context.Context, w http.ResponseWriter, request *sqlite.MemberForwardRequest, body []byte, started *bool, current func(context.Context) error) error {
	check := func() error {
		if err := request.Check(ctx); err != nil {
			return err
		}
		return current(ctx)
	}
	if err := check(); err != nil {
		return err
	}
	deadline, _ := ctx.Deadline()
	c := http.NewResponseController(w)
	if err := c.SetWriteDeadline(deadline); err != nil {
		return err
	}
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { _ = c.SetWriteDeadline(time.Now()); close(done) })
	defer func() {
		if !stop() {
			<-done
		}
	}()
	if err := check(); err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	*started = true
	w.WriteHeader(http.StatusAccepted)
	n, err := w.Write(body)
	if err != nil {
		return err
	}
	if n != len(body) {
		return io.ErrShortWrite
	}
	if err := check(); err != nil {
		return err
	}
	if err := c.Flush(); err != nil {
		return err
	}
	return check()
}
