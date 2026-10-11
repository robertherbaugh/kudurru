package edge

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
)

// Options are the `kudurru edge run|render` inputs that shape a Topology;
// backends arrive separately from the membership source.
type Options struct {
	// ServerID is general.server_id; default the host name.
	ServerID string
	// Listen are the hosts every frontend binds; default 0.0.0.0.
	Listen []string
	// Protocols are the frontends; default Do53 plus every TLS protocol
	// when a certificate is configured.
	Protocols []Protocol
	// Ports overrides a protocol's default port.
	Ports map[Protocol]int
	// CertFile and KeyFile are the TLS pair (FindTLS).
	CertFile, KeyFile string
	// PoolResolver and PoolAuthoritative are the pool policies; default
	// chashed and leastOutstanding.
	PoolResolver, PoolAuthoritative string
	// ResolverCacheSize is the resolver packet cache size; default 200000.
	ResolverCacheSize int
	// AuthoritativeCache enables a short cache on the authoritative pool
	// with AuthoritativeCacheTTL seconds of max TTL (default 30).
	AuthoritativeCache    bool
	AuthoritativeCacheTTL int
	// API and Console are loopback host:port; default 127.0.0.1:8083 and
	// 127.0.0.1:5199.
	API, Console string
	// ProxyProtocolSecret is the TLV 0xE0 value.
	ProxyProtocolSecret string
	// ACL, TransferSources and AuthoritativeZones map to Topology.
	ACL                []string
	TransferSources    []string
	AuthoritativeZones []string
	DynBlocks          DynBlocks
	EBPF               EBPF
	// Threads is the Do53 listener thread count; default 2.
	Threads int
}

// Build returns the Topology for o, the given backends and credentials.
// The result is normalized and validated.
func Build(o Options, backends []Backend, creds Credentials) (*Topology, error) {
	if o.ServerID == "" {
		o.ServerID, _ = os.Hostname()
	}
	if len(o.Listen) == 0 {
		o.Listen = []string{"0.0.0.0"}
	}
	for _, host := range o.Listen {
		if _, err := netip.ParseAddr(host); err != nil {
			return nil, fmt.Errorf("edge: listen address %q: %w (dnsdist binds to IP addresses)", host, err)
		}
	}
	if len(o.Protocols) == 0 {
		o.Protocols = []Protocol{Do53}
		if o.CertFile != "" {
			o.Protocols = slices.Clone(AllProtocols)
		}
	}
	if o.PoolResolver == "" {
		o.PoolResolver = "chashed"
	}
	if o.PoolAuthoritative == "" {
		o.PoolAuthoritative = "leastOutstanding"
	}
	if o.API == "" {
		o.API = "127.0.0.1:8083"
	}
	if o.Console == "" {
		o.Console = "127.0.0.1:5199"
	}
	if o.Threads == 0 {
		o.Threads = 2
	}
	t := &Topology{
		ServerID:            o.ServerID,
		ACL:                 o.ACL,
		Console:             Console{Address: o.Console},
		Webserver:           Webserver{Address: o.API},
		HealthCheck:         DefaultHealthCheck(),
		Rules:               Rules{TransferSources: o.TransferSources, AuthoritativeZones: o.AuthoritativeZones},
		DynBlocks:           o.DynBlocks,
		EBPF:                o.EBPF,
		ProxyProtocolSecret: o.ProxyProtocolSecret,
	}
	creds.Apply(t)
	for _, p := range o.Protocols {
		if p.TLS() && o.CertFile == "" {
			return nil, fmt.Errorf("edge: %s needs a certificate (--tls-dir)", p)
		}
		port := p.DefaultPort()
		if v, ok := o.Ports[p]; ok && v > 0 {
			port = v
		}
		for _, host := range o.Listen {
			b := Bind{Address: net.JoinHostPort(host, strconv.Itoa(port)), Protocol: p}
			if p.TLS() {
				b.CertFile, b.KeyFile = o.CertFile, o.KeyFile
			}
			if p == Do53 {
				b.ReusePort, b.Threads = true, o.Threads
			}
			t.Binds = append(t.Binds, b)
		}
	}
	resolver := DefaultResolverCache()
	if o.ResolverCacheSize > 0 {
		resolver.Size = o.ResolverCacheSize
	}
	t.Pools = []Pool{{Name: PoolResolver, Policy: o.PoolResolver, Cache: resolver}, {Name: PoolAuthoritative, Policy: o.PoolAuthoritative}}
	if o.AuthoritativeCache {
		t.Pools[1].Cache = AuthoritativeCache(o.AuthoritativeCacheTTL)
	}
	for _, b := range backends {
		b.ProxyProtocol = true
		t.Backends = append(t.Backends, b)
	}
	t.Normalize()
	if err := t.Validate(); err != nil {
		return nil, err
	}
	return t, nil
}

// ErrNoTLS is returned by FindTLS when the directory holds no known pair.
var ErrNoTLS = errors.New("edge: no certificate pair found")

// tlsPairs are the file names FindTLS looks for, in order: the
// kubernetes.io/tls Secret layout, Let's Encrypt and a generic pair.
var tlsPairs = [][2]string{{"tls.crt", "tls.key"}, {"fullchain.pem", "privkey.pem"}, {"cert.pem", "key.pem"}}

// FindTLS returns the certificate and key paths in dir.
func FindTLS(dir string) (cert, key string, err error) {
	for _, p := range tlsPairs {
		c, k := filepath.Join(dir, p[0]), filepath.Join(dir, p[1])
		if fileExists(c) && fileExists(k) {
			return c, k, nil
		}
	}
	return "", "", fmt.Errorf("%w in %s (want tls.crt+tls.key, fullchain.pem+privkey.pem or cert.pem+key.pem)", ErrNoTLS, dir)
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
