package internalapi

import (
	"context"
	"crypto/tls"
	"net/http"
	"strings"
	"time"

	"github.com/robertherbaugh/kudurru/internal/cluster/memberidentity"
)

const memberNameSuffix = ".member.kudurru.invalid"

// EnrolledListener keeps member mutual TLS and routes separate from legacy
// status/list credentials on the same socket. The identity is loaded once;
// peers must read current complete enrollment under the supplied handshake
// context. This TLS observation supplies no request or domain authorization.
type EnrolledListener struct {
	identity *memberidentity.Identity
	name     string
	peers    func(context.Context) ([]memberidentity.Trust, error)
	handler  http.Handler
}

// NewEnrolledListener requires an already loaded private identity and an
// independently admitted member handler. It never creates or repairs identity.
// An empty peer set is valid startup state but refuses every member handshake.
func NewEnrolledListener(identity *memberidentity.Identity, peers func(context.Context) ([]memberidentity.Trust, error), handler http.Handler) (*EnrolledListener, error) {
	if identity == nil || !identity.Trust().ValidAt(time.Now()) || peers == nil || handler == nil {
		return nil, memberidentity.ErrIdentity
	}
	return &EnrolledListener{identity: identity, name: identity.Trust().MemberID() + memberNameSuffix, peers: peers, handler: handler}, nil
}

func (e *EnrolledListener) tlsConfig(legacy *tls.Config) *tls.Config {
	combined := legacy.Clone()
	combined.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		if hello.ServerName != e.name {
			if strings.HasSuffix(hello.ServerName, memberNameSuffix) {
				return nil, memberidentity.ErrIdentity
			}
			return legacy, nil
		}
		ctx, cancel := context.WithTimeout(hello.Context(), 5*time.Second)
		defer cancel()
		peers, err := e.peers(ctx)
		if err != nil {
			return nil, err
		}
		config, err := e.identity.ServerTLS(peers)
		if err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		config.NextProtos = []string{"h2", "http/1.1"}
		return config, nil
	}
	return combined
}

func (e *EnrolledListener) routes(legacy http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Route from actual TLS SNI, never the HTTP Host header. Enrolled
		// requests do not enter the legacy body/token guard or its route set.
		if r.TLS != nil && r.TLS.ServerName == e.name {
			if !r.TLS.HandshakeComplete || r.TLS.Version < tls.VersionTLS13 || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates) == 0 {
				http.Error(w, "member authentication required", http.StatusUnauthorized)
				return
			}
			e.handler.ServeHTTP(w, r)
			return
		}
		legacy.ServeHTTP(w, r)
	})
}
