package auth

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"strconv"
)

// PeerCred is the kernel-reported identity of a unix-socket peer
// (SO_PEERCRED on Linux).
type PeerCred struct {
	PID int32
	UID uint32
	GID uint32
}

type peerCtxKey struct{}

// WithPeer returns a context carrying the peer credentials of the
// connection the request arrived on.
func WithPeer(ctx context.Context, c PeerCred) context.Context {
	return context.WithValue(ctx, peerCtxKey{}, c)
}

// PeerFrom returns the peer credentials set by WithPeer (or
// PeerConnContext).
func PeerFrom(ctx context.Context) (PeerCred, bool) {
	c, ok := ctx.Value(peerCtxKey{}).(PeerCred)
	return c, ok
}

// ErrNoPeerCred is returned by ReadPeerCred for connections that are not
// unix sockets or on platforms without SO_PEERCRED.
var ErrNoPeerCred = errors.New("auth: peer credentials unavailable")

// PeerConnContext is an http.Server ConnContext for a unix-socket listener:
// it records the peer's SO_PEERCRED credentials in the connection context,
// where UnixPeer finds them. Connections whose credentials cannot be read
// get none and are therefore never authenticated as the local root.
func PeerConnContext(ctx context.Context, c net.Conn) context.Context {
	if cred, err := ReadPeerCred(c); err == nil {
		return WithPeer(ctx, cred)
	}
	return ctx
}

// PeerPolicy decides which unix-socket peers are the local root (§8.7):
// uid 0, the uid the server runs as (65532 in the distroless image, which is
// what `docker exec` and `kubectl exec` get without --user), or a member of
// the configured gid.
type PeerPolicy struct {
	// GID additionally admits peers whose primary gid matches; negative
	// disables it.
	GID int
	// ServerUID is the server's own uid; default os.Getuid().
	ServerUID int
}

// Allowed reports whether c is admitted.
func (p PeerPolicy) Allowed(c PeerCred) bool {
	uid := p.ServerUID
	if uid == 0 { // unset; uid 0 is admitted below anyway
		uid = os.Getuid()
	}
	switch {
	case c.UID == 0:
		return true
	case uid >= 0 && int64(c.UID) == int64(uid):
		return true
	case p.GID >= 0 && int64(c.GID) == int64(p.GID):
		return true
	}
	return false
}

// LocalRootPrincipal is the principal of an admitted unix-socket peer: the
// break-glass implicit admin of `kudurru admin` (Decision.Path "local").
func LocalRootPrincipal(c PeerCred) Principal {
	uid := strconv.FormatUint(uint64(c.UID), 10)
	return Principal{
		Kind: KindLocalRoot, ID: "unix:" + uid, Name: "local-root (uid " + uid + ")",
		Provider: "unix", AuthMethod: MethodUnix, BuiltinAdmin: true,
	}
}

// UnixPeer is the unix-peer step of the chain: a request that arrived on
// the admin socket (PeerConnContext) from an admitted peer is the local
// root. A request with peer credentials that the policy refuses is
// rejected with 403; requests from TCP listeners carry none and pass on.
type UnixPeer struct {
	Policy PeerPolicy
}

// Authenticate implements Authenticator.
func (u UnixPeer) Authenticate(_ http.ResponseWriter, r *http.Request) (Principal, *http.Request, error) {
	c, ok := PeerFrom(r.Context())
	if !ok {
		return Principal{}, nil, ErrNoCredentials
	}
	if !u.Policy.Allowed(c) {
		return Principal{}, nil, &Error{Status: http.StatusForbidden, Key: "forbidden",
			Message: "this unix user may not use the admin socket", Hint: "run as root, as the kudurru user, or as a member of auth.adminSocket.group"}
	}
	return LocalRootPrincipal(c), r, nil
}

// InternalToken is the last step of the chain: a peer node presenting the
// internal-listener bearer token (internal/cluster/internalapi
// Credentials.ValidToken) is a Node principal with the read-only Implicit
// grants (viewer and auditor), enough to fan out stats and query-log reads.
// Any other bearer value is left to the earlier steps' verdicts, so this
// step only claims tokens it validates.
type InternalToken struct {
	// Valid reports in constant time whether a bearer value is an accepted
	// internal token. Required.
	Valid func(token string) bool
	// Grants are the Node principal's implicit grants in wire form;
	// default viewer and auditor.
	Grants []string
}

// Authenticate implements Authenticator.
func (t InternalToken) Authenticate(_ http.ResponseWriter, r *http.Request) (Principal, *http.Request, error) {
	tok := BearerToken(r)
	if tok == "" || t.Valid == nil || !t.Valid(tok) {
		return Principal{}, nil, ErrNoCredentials
	}
	grants := t.Grants
	if grants == nil {
		grants = []string{"auditor", "viewer"}
	}
	return Principal{Kind: KindNode, ID: "node:internal", Name: "kudurru-node", Provider: "internal",
		AuthMethod: MethodInternal, Implicit: grants}, r, nil
}
