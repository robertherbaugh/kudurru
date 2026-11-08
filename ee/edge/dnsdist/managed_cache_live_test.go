package dnsdist

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/ee/edge"
	"github.com/robertherbaugh/kudurru/internal/cluster"
	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
	"github.com/robertherbaugh/kudurru/internal/dnsserver/proxyproto"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/stages/ingress"
)

type managedLiveZones struct{ endpoint cluster.Endpoint }

func (z managedLiveZones) Zones(context.Context, string) (internalapi.ZoneList, error) {
	o := resolverFixture(time.Now())
	st := o.results[0].zones.Resolver
	st.Node = z.endpoint.Name
	return internalapi.ZoneList{Node: z.endpoint.Name, Resolver: st}, nil
}

func managedCacheLiveBackend(t *testing.T, secret []byte) (cluster.Endpoint, *atomic.Int64) {
	t.Helper()
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	udp, err := net.ListenPacket("udp", tcp.Addr().String())
	if err != nil {
		_ = tcp.Close()
		t.Fatal(err)
	}
	policy, err := proxyproto.NewPolicy([]string{"127.0.0.1"}, secret)
	if err != nil {
		t.Fatal(err)
	}
	ep := cluster.Endpoint{Name: "managed-cache-node", Address: tcp.Addr().String(), InternalAddress: "127.0.0.1:1", Ready: true, Serving: true}
	calls := new(atomic.Int64)
	health := &ingress.Stage{NodeID: ep.Name, Epoch: func() uint64 { return 1 }}
	handler := dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(q)
		if len(q.Question) == 1 && q.Question[0].Qtype == dns.TypeTXT {
			response, err := health.Serve(&pipeline.Request{Ctx: context.Background(), Msg: q}, func(req *pipeline.Request) (*pipeline.Response, error) {
				return pipeline.Reply(req, dns.RcodeNameError, pipeline.StatusForwarded), nil
			})
			if err == nil {
				m = response.Msg
			} else {
				m.SetRcode(q, dns.RcodeServerFailure)
			}
		} else if len(q.Question) == 1 && q.Question[0].Qtype == dns.TypeA {
			if _, ok := w.RemoteAddr().(*proxyproto.Addr); !ok {
				m.SetRcode(q, dns.RcodeRefused)
			} else {
				n := calls.Add(1)
				if n < 1 || n > 254 {
					t.Error("fixture exceeded its bounded distinct answer space")
					m.SetRcode(q, dns.RcodeServerFailure)
					_ = w.WriteMsg(m)
					return
				}
				name := q.Question[0].Name
				if name == "alias.auth.example." {
					m.Answer = append(m.Answer, &dns.CNAME{Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeCNAME, Class: dns.ClassINET, Ttl: 120}, Target: "outside.example."})
					name = "outside.example."
				}
				m.Answer = append(m.Answer, &dns.A{Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 120}, A: net.IPv4(192, 0, 2, byte(n))})
			}
		}
		_ = w.WriteMsg(m)
	})
	servers := []*dns.Server{{Listener: &proxyproto.Listener{Listener: tcp, Policy: policy}, Handler: handler}, {PacketConn: &proxyproto.PacketConn{PacketConn: udp, Policy: policy}, Handler: handler}}
	for _, s := range servers {
		done := make(chan struct{})
		go func() { defer close(done); _ = s.ActivateAndServe() }()
		t.Cleanup(func() { _ = s.Shutdown(); <-done })
	}
	return ep, calls
}

// This opt-in fixture uses only the retained pinned external dnsdist image.
// It qualifies the installed edge policy, not backend managed activation.
func TestManagedCacheLivePinnedTransition(t *testing.T) {
	if os.Getenv("KUDURRU_F28_EDGE_LIVE") != "1" {
		t.Skip("set KUDURRU_F28_EDGE_LIVE=1 for owned pinned dnsdist managed-cache acceptance")
	}
	parent := os.Getenv("KUDURRU_F28_EDGE_WORKDIR")
	if parent == "" {
		t.Fatal("owned scratch parent required")
	}
	for _, renderer := range []Renderer{YAMLRenderer{}, LuaRenderer{}} {
		t.Run(renderer.Name(), func(t *testing.T) {
			creds, err := edge.NewCredentials()
			if err != nil {
				t.Fatal(err)
			}
			endpoint, calls := managedCacheLiveBackend(t, []byte(creds.Password))
			listen := freePort(t)
			_, portText, _ := net.SplitHostPort(listen)
			port, err := strconv.Atoi(portText)
			if err != nil {
				t.Fatal(err)
			}
			start := func(managed bool) (*Supervisor, func(), string) {
				dir, err := os.MkdirTemp(parent, "managed-edge-")
				if err != nil {
					t.Fatal(err)
				}
				name := "kudurru-f28-edge-" + strconv.FormatInt(time.Now().UnixNano(), 36)
				fixture, err := json.Marshal(map[string]string{"name": name, "image": consoleLabImage})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "fixture.json"), fixture, 0o600); err != nil { //nolint:gosec // fresh private fixture below the explicitly owned scratch parent
					t.Fatal(err)
				}
				wrapper, err := os.ReadFile("testdata/console-client.py")
				if err != nil {
					t.Fatal(err)
				}
				binary := filepath.Join(dir, "dnsdist-client")
				if err := os.WriteFile(binary, wrapper, 0o700); err != nil { //nolint:gosec // fixed tracked wrapper in fresh private fixture must be executable
					t.Fatal(err)
				}
				t.Cleanup(func() {
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					_ = exec.CommandContext(ctx, "docker", "--host", "unix:///tmp/kudurru-lab-docker.sock", "rm", "--force", name).Run() //nolint:gosec // fixed daemon and only this generated owned fixture resource
				})
				sup, err := New(Config{Renderer: renderer, Options: edge.Options{ManagedCacheBypass: managed, AuthoritativeCache: true, AuthoritativeZones: []string{"auth.example."}, Listen: []string{"127.0.0.1"}, Protocols: []edge.Protocol{edge.Do53}, Ports: map[edge.Protocol]int{edge.Do53: port}, API: freePort(t), Console: freePort(t), ProxyProtocolSecret: creds.Password}, Source: &cluster.StaticSource{Endpoints: []cluster.Endpoint{endpoint}}, Zones: managedLiveZones{endpoint}, ConfigDir: filepath.Join(dir, "config"), Binary: binary, ChildEnv: []string{"KUDURRU_F43_LAB=" + dir}, ZonePoll: 100 * time.Millisecond, StatusPoll: 100 * time.Millisecond, Debounce: 100 * time.Millisecond, StopTimeout: 10 * time.Second, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(t.Context())
				done := make(chan error, 1)
				go func() { done <- sup.Run(ctx) }()
				stopped := false
				stop := func() {
					if stopped {
						return
					}
					stopped = true
					cancel()
					select {
					case err := <-done:
						if err != nil {
							t.Error("supervisor completion", err)
						}
					case <-time.After(15 * time.Second):
						t.Error("supervisor did not join")
					}
				}
				t.Cleanup(stop)
				labWait(t, "actual child ready", func() bool { ok, _ := sup.Ready(); return ok })
				if labPID(t, name) == "" {
					t.Fatal("actual pinned child not observed")
				}
				return sup, stop, name
			}
			ask := func(name string) string {
				q := new(dns.Msg)
				q.SetQuestion(name, dns.TypeA)
				m, _, err := (&dns.Client{Timeout: 2 * time.Second}).Exchange(q, listen)
				if err != nil || m == nil || m.Rcode != dns.RcodeSuccess || len(m.Answer) == 0 {
					t.Fatal("actual edge response unavailable", err)
				}
				a, ok := m.Answer[len(m.Answer)-1].(*dns.A)
				if !ok {
					t.Fatal("missing final A")
				}
				return a.A.String()
			}
			static, stopStatic, _ := start(false)
			labWait(t, "both static caches qualified", func() bool {
				static.mu.Lock()
				defer static.mu.Unlock()
				return static.cacheMode == "qualified" && static.authoritativeCacheMode == "qualified"
			})
			for _, name := range []string{"resolver.example.", "authoritative.auth.example.", "alias.auth.example."} {
				before := calls.Load()
				first := ask(name)
				second := ask(name)
				if first != second || calls.Load() != before+1 {
					t.Fatal("static original did not actually warm and hit cache", name, calls.Load()-before)
				}
			}
			stopStatic() // old warmed child is joined before the bypass target starts
			managed, stopManaged, name := start(true)
			verify := func() {
				t.Helper()
				status, err := managed.api.Status(t.Context())
				if err != nil {
					t.Fatal("actual child status unavailable", err)
				}
				pools := 0
				for _, pool := range status.Pools {
					if pool.Name == edge.PoolResolver || pool.Name == edge.PoolAuthoritative {
						pools++
						if pool.CacheSize != 0 {
							t.Fatal("actual managed child has a cache attached", pool.Name)
						}
					}
				}
				if pools != 2 {
					t.Fatal("actual child did not expose both fixed pools")
				}
				for _, qname := range []string{"resolver.example.", "authoritative.auth.example.", "alias.auth.example."} {
					before := calls.Load()
					first := ask(qname)
					second := ask(qname)
					if first == second || calls.Load() != before+2 {
						t.Fatal("managed policy reused a packet cache", qname, calls.Load()-before)
					}
				}
			}
			verify()
			// A valid, previously captured remote observation cannot reattach either pool.
			observation := resolverFixture(time.Now())
			observation.fence, observation.endpoints, observation.eligible = managed.resolverFence()
			observation.results[0].endpoint = endpoint
			observation.results[0].zones.Node = endpoint.Name
			observation.results[0].zones.Resolver.Node = endpoint.Name
			managed.offerResolverObservation(observation)
			managed.noteResolverSource(nil, true)
			if ready, _ := managed.Ready(); ready {
				t.Fatal("membership loss retained installed readiness")
			}
			managed.noteResolverSource([]cluster.Endpoint{endpoint}, false)
			labWait(t, "new source acknowledgment", func() bool { ok, _ := managed.Ready(); return ok })
			verify()
			oldPID := labPID(t, name)
			if err := managed.Signal(syscall.SIGUSR1); err != nil {
				t.Fatal(err)
			}
			labWait(t, "actual replacement bypass child", func() bool { pid := labPID(t, name); ok, _ := managed.Ready(); return pid != "" && pid != oldPID && ok })
			managed.offerResolverObservation(observation)
			verify()
			// Lose the reply only after the actual daemon applied the fixed
			// unsets. Unknown completion must retire this exact child, not be
			// promoted to installed proof or replayed into it.
			oldPID = labPID(t, name)
			fault := filepath.Join(filepath.Dir(managed.cfg.ConfigDir), "lose-ack-none")
			if err := os.WriteFile(fault, nil, 0o600); err != nil { //nolint:gosec // exact owned fixture fault consumed by the tracked wrapper
				t.Fatal(err)
			}
			managed.noteResolverSource([]cluster.Endpoint{endpoint}, false)
			if ready, _ := managed.Ready(); ready {
				t.Fatal("new source reused previous acknowledgment")
			}
			labWait(t, "unknown acknowledgment retires actual child", func() bool {
				pid := labPID(t, name)
				ok, _ := managed.Ready()
				return pid != "" && pid != oldPID && ok
			})
			if _, err := os.Stat(fault); !os.IsNotExist(err) {
				t.Fatal("actual post-effect acknowledgment fault was not consumed", err)
			}
			verify()
			// Restart the entire supervisor against the SAME durable directory.
			// A previous child acknowledgment cannot replace the new console
			// acknowledgment, and disabling bypass must refuse before any child.
			managed.mu.Lock()
			identity := managed.configState.bypassID
			managed.mu.Unlock()
			stopManaged()
			restartConfig := managed.cfg
			restartConfig.Registerer, restartConfig.Gatherer = nil, nil
			restartConfig.Options.ManagedCacheBypass = false
			disabled, err := New(restartConfig)
			if err != nil {
				t.Fatal(err)
			}
			refusalContext, cancelRefusal := context.WithTimeout(t.Context(), 3*time.Second)
			err = disabled.Run(refusalContext)
			cancelRefusal()
			if err == nil || !strings.Contains(err.Error(), "requires managed cache bypass") || disabled.child != nil {
				t.Fatal("supervisor restart disabled retained bypass or launched a child", err)
			}
			restartConfig.Options.ManagedCacheBypass = true
			managed, err = New(restartConfig)
			if err != nil {
				t.Fatal(err)
			}
			if ready, _ := managed.Ready(); ready {
				t.Fatal("restarted supervisor inherited installed readiness")
			}
			restartContext, cancelRestart := context.WithCancel(t.Context())
			restartDone := make(chan error, 1)
			go func() { restartDone <- managed.Run(restartContext) }()
			t.Cleanup(func() {
				cancelRestart()
				select {
				case err := <-restartDone:
					if err != nil {
						t.Error("restarted supervisor completion", err)
					}
				case <-time.After(15 * time.Second):
					t.Error("restarted supervisor did not join")
				}
			})
			labWait(t, "restarted supervisor and actual bypass child", func() bool { ok, _ := managed.Ready(); return ok })
			managed.mu.Lock()
			sameIdentity := managed.configState != nil && managed.configState.bypassID == identity
			managed.mu.Unlock()
			if !sameIdentity || labPID(t, name) == "" {
				t.Fatal("restarted child lost durable deployment identity")
			}
			managed.offerResolverObservation(observation)
			verify()
		})
	}
}
