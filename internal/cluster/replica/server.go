package replica

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// ServerOptions supplies the authoritative store, peer policy and command owners.
type ServerOptions struct {
	Store       *sqlite.Store
	Credentials *internalapi.Credentials
	// AuthorizePeer is mandatory enrollment/cluster validation, not public RBAC.
	// Shared credentials alone do not bind an individual member UUID.
	AuthorizePeer                                  PeerAuthorizer
	Commands                                       *CommandService
	MaxImageBytes, MaxCommandBytes, MaxResultBytes int64
	MaxConcurrent                                  int
	Timeout                                        time.Duration
}

// Server provides independently guarded replication routes on an internal mux.
type Server struct {
	opts  ServerOptions
	slots chan struct{}
}

// NewServer requires enrollment policy in addition to shared cluster credentials.
func NewServer(o ServerOptions) (*Server, error) {
	if o.Store == nil || o.Credentials == nil || o.AuthorizePeer == nil {
		return nil, errors.New("replica: store, credentials and enrolled peer authorizer required")
	}
	if o.MaxImageBytes <= 0 {
		o.MaxImageBytes = replication.Limits{}.Defaults().MaxBytes
	}
	if o.MaxCommandBytes <= 0 {
		o.MaxCommandBytes = DefaultMaxCommandBytes
	}
	if o.MaxResultBytes <= 0 {
		o.MaxResultBytes = DefaultMaxResultBytes
	}
	if o.MaxConcurrent <= 0 {
		o.MaxConcurrent = 4
	}
	if o.Timeout <= 0 {
		o.Timeout = 30 * time.Second
	}
	return &Server{opts: o, slots: make(chan struct{}, o.MaxConcurrent)}, nil
}

// Mount registers routes with their OWN peer guard. Root must dispatch them
// outside the existing internalapi guard's GET/event-only body policy.
func (s *Server) Mount(mux *http.ServeMux) {
	mux.Handle(PathImage, s.guard(http.HandlerFunc(s.image)))
	mux.Handle(PathWrite, s.guard(http.HandlerFunc(s.write)))
}

// Handler returns a mux containing only the guarded replication routes.
func (s *Server) Handler() http.Handler { m := http.NewServeMux(); s.Mount(m); return m }
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store, private")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.TLS == nil || r.TLS.Version < tls.VersionTLS13 {
			s.fail(w, ErrUnauthorized)
			return
		}
		parts := strings.SplitN(r.Header.Get("Authorization"), " ", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || !s.opts.Credentials.ValidToken(parts[1]) {
			s.fail(w, ErrUnauthorized)
			return
		}
		a := s.opts.Store.Authority()
		peer := Peer{MemberID: r.Header.Get(HeaderMember), ClusterID: r.Header.Get(HeaderCluster)}
		if len(r.Header.Values(HeaderMember)) != 1 || len(r.Header.Values(HeaderCluster)) != 1 || !validID(peer.MemberID) || !validID(peer.ClusterID) || peer.ClusterID != a.Term.ClusterID {
			s.fail(w, ErrUnauthorized)
			return
		}
		if err := s.opts.AuthorizePeer(r.Context(), peer, r); err != nil {
			s.fail(w, ErrUnauthorized)
			return
		}
		s.termHeaders(w, a.Term)
		if r.URL.RawQuery != "" {
			s.fail(w, ErrProtocol)
			return
		}
		select {
		case s.slots <- struct{}{}:
			defer func() { <-s.slots }()
		default:
			s.fail(w, ErrUnavailable)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), s.opts.Timeout)
		defer cancel()
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(s.opts.Timeout))
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(s.opts.Timeout))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
func (s *Server) termHeaders(w http.ResponseWriter, t replication.Term) {
	w.Header().Set(HeaderMember, s.opts.Store.Authority().MemberID)
	w.Header().Set(HeaderCluster, t.ClusterID)
	w.Header().Set(HeaderTerm, encodeTerm(t))
}
func (s *Server) fail(w http.ResponseWriter, err error) {
	code, key := problem(err)
	if code == 401 {
		w.Header().Set("WWW-Authenticate", `Bearer realm="kudurru-replica"`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(Problem{Code: key})
}
func (s *Server) requirePrimary(w http.ResponseWriter) (replication.Term, error) {
	a := s.opts.Store.Authority()
	s.termHeaders(w, a.Term)
	if a.Mode != replication.Primary {
		if a.Mode == replication.Fenced || a.Mode == replication.Conflict {
			return a.Term, replication.ErrFenced
		}
		return a.Term, replication.ErrReadOnly
	}
	if _, err := s.opts.Store.WriterToken(); err != nil {
		return a.Term, err
	}
	return a.Term, nil
}
func (s *Server) image(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if r.ContentLength != 0 || len(r.TransferEncoding) > 0 {
		s.fail(w, ErrProtocol)
		return
	}
	term, err := s.requirePrimary(w)
	if err != nil {
		s.fail(w, err)
		return
	}
	image, err := s.opts.Store.ReadGeneration(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	current, err := s.requirePrimary(w)
	if err != nil {
		s.fail(w, err)
		return
	}
	if !current.Same(term) || !image.Term.Same(current) {
		s.fail(w, replication.ErrCAS)
		return
	}
	etag := image.Generation().ETag()
	s.termHeaders(w, image.Term)
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	b, err := json.Marshal(image)
	if err != nil {
		s.fail(w, err)
		return
	}
	if int64(len(b)) > s.opts.MaxImageBytes {
		s.fail(w, replication.ErrTooLarge)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}
func (s *Server) write(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if s.opts.Commands == nil {
		s.fail(w, ErrUnavailable)
		return
	}
	term, err := s.requirePrimary(w)
	if err != nil {
		s.fail(w, err)
		return
	}
	if r.ContentLength > s.opts.MaxCommandBytes {
		s.fail(w, replication.ErrTooLarge)
		return
	}
	if r.Header.Get("Content-Type") != "application/json" {
		s.fail(w, ErrProtocol)
		return
	}
	b, err := readBounded(http.MaxBytesReader(w, r.Body, s.opts.MaxCommandBytes), s.opts.MaxCommandBytes)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			err = replication.ErrTooLarge
		}
		s.fail(w, err)
		return
	}
	var command Command
	if err := decodeStrict(b, &command); err != nil {
		s.fail(w, err)
		return
	}
	if !command.ExpectedTerm.Same(term) {
		s.fail(w, replication.ErrCAS)
		return
	}
	outcome, err := s.opts.Commands.Execute(withForwarded(r.Context()), command)
	if err != nil {
		s.termHeaders(w, s.opts.Store.Authority().Term)
		s.fail(w, err)
		return
	}
	b, err = json.Marshal(outcome)
	if err != nil || int64(len(b)) > s.opts.MaxResultBytes {
		s.fail(w, ErrOutcomeUnknown)
		return
	}
	// Keep the term used by the committed command. A subsequently observed term
	// cannot relabel its outcome; callers may learn newer authority on next pull.
	s.termHeaders(w, term)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}
