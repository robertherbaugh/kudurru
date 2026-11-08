package replica

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// MemberCommandHandler retains original domain/dependency pins around both the
// actual command and emit. It must execute synchronously through SQLiteMemberAtomic
// with the supplied original request, then call emit exactly once before releasing
// those pins. Membership supplies no user permission. Never retain emit, spawn
// work or return a proposal as success; only Store completion permits output.
type MemberCommandHandler func(context.Context, *sqlite.MemberRequest, Command, func(Outcome) error) error

// MemberScopedCommandHandler uses an enclosing command/output coordinator.
// emit accepts only the context issued by this request's active
// WithCommandOutput callback. It writes and flushes synchronously without
// acquiring another Store output scope. Domain proof and dependency exclusions
// remain the caller's obligation; staged outcomes alone cannot acknowledge.
type MemberScopedCommandHandler func(context.Context, *sqlite.MemberRequest, Command, func(context.Context, Outcome) error) error

// MemberServerOptions binds the enrolled HTTP transport to the actual Store.
// With neither executor, image reads remain available and mutations refuse.
// The two executor forms are mutually exclusive.
type MemberServerOptions struct {
	Store                                          *sqlite.Store
	Credentials                                    *internalapi.Credentials
	Execute                                        MemberCommandHandler
	ExecuteScoped                                  MemberScopedCommandHandler
	MaxImageBytes, MaxCommandBytes, MaxResultBytes int64
	MaxConcurrent                                  int
	Timeout                                        time.Duration
}

// MemberServer retains original enrollment through body, commit and output.
// The listener must supply actual TLS connection state. Root owns listener and
// domain assembly; this server never treats shared credentials as enrollment.
type MemberServer struct {
	opts  MemberServerOptions
	slots chan struct{}
}

// NewMemberServer requires a finite original request of at most 30 seconds.
func NewMemberServer(o MemberServerOptions) (*MemberServer, error) {
	if o.Store == nil || o.Credentials == nil || o.Timeout < 0 || o.Timeout > 30*time.Second ||
		o.MaxConcurrent < 0 || o.MaxImageBytes < 0 || o.MaxCommandBytes < 0 || o.MaxResultBytes < 0 ||
		o.Execute != nil && o.ExecuteScoped != nil {
		return nil, ErrProtocol
	}
	if o.Timeout == 0 {
		o.Timeout = 30 * time.Second
	}
	if o.MaxConcurrent == 0 {
		o.MaxConcurrent = 4
	}
	if o.MaxImageBytes == 0 {
		o.MaxImageBytes = replication.Limits{}.Defaults().MaxBytes
	}
	if o.MaxCommandBytes == 0 {
		o.MaxCommandBytes = DefaultMaxCommandBytes
	}
	if o.MaxResultBytes == 0 {
		o.MaxResultBytes = DefaultMaxResultBytes
	}
	return &MemberServer{opts: o, slots: make(chan struct{}, o.MaxConcurrent)}, nil
}

// Mount registers routes with their own original enrolled admission. Root must
// dispatch outside the legacy GET/event-only internalapi guard.
func (s *MemberServer) Mount(mux *http.ServeMux) {
	mux.HandleFunc(PathImage, s.serve)
	mux.HandleFunc(PathWrite, s.serve)
}

// Handler returns only the enrolled image and command routes.
func (s *MemberServer) Handler() http.Handler {
	mux := http.NewServeMux()
	s.Mount(mux)
	return mux
}

func (s *MemberServer) serve(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), s.opts.Timeout)
	defer cancel()
	w.Header().Set("Cache-Control", "no-store, private")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Admission owns its slot before any enrollment or body I/O and retains it
	// through explicit emission. An unsupported transport cannot execute work.
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		writeProblem(w, ErrUnavailable)
		return
	}
	deadline, _ := ctx.Deadline()
	controller := http.NewResponseController(w)
	if ctx.Err() != nil || controller.SetReadDeadline(deadline) != nil || controller.SetWriteDeadline(deadline) != nil {
		writeProblem(w, ErrUnavailable)
		return
	}
	parts := strings.SplitN(r.Header.Get("Authorization"), " ", 2)
	if len(r.Header.Values("Authorization")) != 1 || len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || !s.opts.Credentials.ValidToken(parts[1]) ||
		len(r.Header.Values(HeaderMember)) != 1 || len(r.Header.Values(HeaderCluster)) != 1 ||
		!validID(r.Header.Get(HeaderMember)) || !validID(r.Header.Get(HeaderCluster)) {
		writeProblem(w, ErrUnauthorized)
		return
	}
	if r.URL.RawQuery != "" {
		writeProblem(w, ErrProtocol)
		return
	}
	request, err := s.opts.Store.CaptureMemberRequest(ctx, r.Header.Get(HeaderCluster), r.Header.Get(HeaderMember), r.TLS)
	if err != nil {
		writeProblem(w, ErrUnauthorized)
		return
	}
	ctx, cancelPeer := context.WithDeadline(ctx, request.Deadline())
	defer cancelPeer()
	if ctx.Err() != nil || controller.SetReadDeadline(request.Deadline()) != nil || controller.SetWriteDeadline(request.Deadline()) != nil {
		writeProblem(w, ErrUnavailable)
		return
	}
	r = r.WithContext(ctx)
	if r.URL.Path == PathImage {
		s.image(w, r, request)
	} else {
		s.write(w, r, request)
	}
}

func (s *MemberServer) image(w http.ResponseWriter, r *http.Request, request *sqlite.MemberRequest) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
		writeProblem(w, ErrProtocol)
		return
	}
	image, err := request.ReadImage()
	if err != nil {
		writeProblem(w, ErrUnavailable)
		return
	}
	etag := image.Generation().ETag()
	status := http.StatusOK
	var body []byte
	if r.Header.Get("If-None-Match") == etag {
		status = http.StatusNotModified
	} else {
		body, err = json.Marshal(image)
		if err != nil || int64(len(body)) > s.opts.MaxImageBytes {
			writeProblem(w, replication.ErrTooLarge)
			return
		}
	}
	started := false
	if err := memberEmit(r.Context(), w, request, request.WithOutput, status, etag, body, &started); err != nil {
		if started {
			panic(http.ErrAbortHandler)
		}
		writeProblem(w, ErrUnavailable)
	}
}

func (s *MemberServer) write(w http.ResponseWriter, r *http.Request, request *sqlite.MemberRequest) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if s.opts.Execute == nil && s.opts.ExecuteScoped == nil {
		writeProblem(w, ErrUnavailable)
		return
	}
	if r.ContentLength > s.opts.MaxCommandBytes {
		writeProblem(w, replication.ErrTooLarge)
		return
	}
	if r.Header.Get("Content-Type") != "application/json" {
		writeProblem(w, ErrProtocol)
		return
	}
	body, err := readBounded(http.MaxBytesReader(w, r.Body, s.opts.MaxCommandBytes), s.opts.MaxCommandBytes)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			err = replication.ErrTooLarge
		}
		writeProblem(w, err)
		return
	}
	var command Command
	if err := decodeStrict(body, &command); err != nil {
		writeProblem(w, err)
		return
	}
	if err := command.validate(time.Now(), DefaultCommandLifetime); err != nil {
		writeProblem(w, err)
		return
	}
	if !command.ExpectedTerm.Same(request.OriginalTerm()) {
		writeProblem(w, replication.ErrCAS)
		return
	}
	ctx, cancel := context.WithDeadline(withForwarded(r.Context()), command.NotAfter)
	defer cancel()
	started, emitted, attempted := false, false, false
	var outputErr error
	emit := func(scope context.Context, out Outcome) (result error) {
		defer func() {
			if result != nil {
				outputErr = result
			}
		}()
		if attempted {
			return ErrProtocol
		}
		attempted = true
		if err := out.validate(s.opts.MaxResultBytes); err != nil {
			return err
		}
		encoded, err := json.Marshal(out)
		if err != nil {
			return err
		}
		withOutput := request.WithCommandOutput
		if s.opts.ExecuteScoped != nil {
			withOutput = func(write func(context.Context) error) error {
				if err := request.CheckCommandOutput(scope); err != nil {
					return err
				}
				if err := write(scope); err != nil {
					return err
				}
				return request.CheckCommandOutput(scope)
			}
		}
		if err := memberEmit(ctx, w, request, withOutput, http.StatusOK, "", encoded, &started); err != nil {
			return err
		}
		emitted = true
		return nil
	}
	if s.opts.ExecuteScoped != nil {
		err = s.opts.ExecuteScoped(ctx, request, command, emit)
	} else {
		err = s.opts.Execute(ctx, request, command, func(out Outcome) error { return emit(nil, out) })
	}
	if err != nil || outputErr != nil || !emitted {
		// The callback may have committed before output admission failed. Do
		// not serialize an acknowledgment or ordinary refusal after dropping its
		// domain pins, and do not let net/http flush a partially buffered body.
		panic(http.ErrAbortHandler)
	}
}

func memberEmit(ctx context.Context, w http.ResponseWriter, request *sqlite.MemberRequest, withOutput func(func(context.Context) error) error, status int, etag string, body []byte, started *bool) error {
	return withOutput(func(original context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := original.Err(); err != nil {
			return err
		}
		deadline, _ := original.Deadline()
		if limit, finite := ctx.Deadline(); finite && limit.Before(deadline) {
			deadline = limit
		}
		if !time.Now().Before(deadline) {
			return context.DeadlineExceeded
		}
		controller := http.NewResponseController(w)
		if err := controller.SetWriteDeadline(deadline); err != nil {
			return err
		}
		// Install the original deadline before cancellation can shorten it.
		// Either parent must interrupt active Write/Flush, and both callbacks
		// must finish before the enclosing authority/dependency pins release.
		interrupt := sync.OnceFunc(func() { _ = controller.SetWriteDeadline(time.Now()) })
		for _, parent := range []context.Context{ctx, original} {
			done := make(chan struct{})
			stop := context.AfterFunc(parent, func() { interrupt(); close(done) })
			defer func() {
				if !stop() {
					<-done
				}
			}()
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := original.Err(); err != nil {
			return err
		}
		a := request.OriginalTerm()
		w.Header().Set(HeaderMember, a.PrimaryID)
		w.Header().Set(HeaderCluster, a.ClusterID)
		w.Header().Set(HeaderTerm, encodeTerm(a))
		if etag != "" {
			w.Header().Set("ETag", etag)
		}
		w.Header().Set("Content-Type", "application/json")
		*started = true
		w.WriteHeader(status)
		if len(body) != 0 {
			n, err := w.Write(body)
			if err != nil {
				return err
			}
			if n != len(body) {
				return io.ErrShortWrite
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := original.Err(); err != nil {
			return err
		}
		if err := controller.Flush(); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := original.Err(); err != nil {
			return err
		}
		if !time.Now().Before(deadline) {
			return context.DeadlineExceeded
		}
		return nil
	})
}
