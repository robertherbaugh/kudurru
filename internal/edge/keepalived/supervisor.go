package keepalived

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/robertherbaugh/kudurru/internal/edge"
)

// Event is a VIP state transition observed through the FIFO state file.
// The supervisor logs it as event `vip.master_changed`; OnEvent hooks
// forward it to audit (`cluster.vip.changed`) and the Events API.
type Event struct {
	VIP      string
	Node     string
	State    string
	Previous string
	Priority int
	At       time.Time
}

// SupervisorConfig configures `kudurru vip run`.
type SupervisorConfig struct {
	// Config is rendered into ConfigPath.
	Config Config
	// ConfigPath is keepalived.conf; default
	// /etc/keepalived/keepalived.conf.
	ConfigPath string
	// Binary is the keepalived executable; default "keepalived".
	Binary string
	// ChildEnv is appended to keepalived's environment (tests).
	ChildEnv []string
	// PeersFile, when set, is watched: one peer address per line; a
	// change re-renders and SIGHUPs keepalived (the Kubernetes DaemonSet
	// mounts the Node list here).
	PeersFile string
	// StatePoll is the FIFO state file poll interval; default 1 s.
	StatePoll time.Duration
	// StopTimeout bounds keepalived's SIGTERM grace period (it releases
	// the VIPs); default 15 s, under compose's stop_grace_period 20 s.
	StopTimeout time.Duration
	// OnEvent receives every VIP transition; it must not block.
	OnEvent func(Event)
	// Logger, Registerer and Gatherer default to slog.Default() and a
	// private registry.
	Logger     *slog.Logger
	Registerer prometheus.Registerer
	Gatherer   prometheus.Gatherer
}

// Supervisor runs keepalived: render → `keepalived -t -f candidate` →
// atomic swap (previous file kept) → child; re-render → check → swap →
// SIGHUP. It never changes a VRID.
type Supervisor struct {
	cfg SupervisorConfig
	log *slog.Logger

	master   *prometheus.GaugeVec
	priority *prometheus.GaugeVec
	reloads  *prometheus.CounterVec
	gatherer prometheus.Gatherer

	mu      sync.Mutex
	current Config
	data    []byte
	child   *edge.Child
	state   StateFile
}

// NewSupervisor validates cfg and returns a Supervisor.
func NewSupervisor(cfg SupervisorConfig) (*Supervisor, error) {
	if cfg.ConfigPath == "" {
		cfg.ConfigPath = "/etc/keepalived/keepalived.conf"
	}
	if cfg.Binary == "" {
		cfg.Binary = "keepalived"
	}
	if cfg.StatePoll <= 0 {
		cfg.StatePoll = time.Second
	}
	if cfg.StopTimeout <= 0 {
		cfg.StopTimeout = 15 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Registerer == nil {
		reg := prometheus.NewRegistry()
		cfg.Registerer, cfg.Gatherer = reg, reg
	}
	if cfg.Gatherer == nil {
		cfg.Gatherer = prometheus.DefaultGatherer
	}
	if err := cfg.Config.Validate(); err != nil {
		return nil, fmt.Errorf("keepalived: %w", err)
	}
	s := &Supervisor{
		cfg: cfg,
		log: cfg.Logger.With("component", "vip"),
		master: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kudurru_vip_master", Help: "1 while this node holds the VIP (VRRP MASTER), from the keepalived notify FIFO.",
		}, []string{"vip"}),
		priority: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kudurru_vip_priority", Help: "Last VRRP priority keepalived reported for the VIP.",
		}, []string{"vip"}),
		reloads: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kudurru_vip_reloads_total", Help: "keepalived re-renders by result (ok, unchanged, invalid, check_failed).",
		}, []string{"result"}),
		gatherer: cfg.Gatherer,
	}
	for _, c := range []prometheus.Collector{s.master, s.priority, s.reloads} {
		if err := cfg.Registerer.Register(c); err != nil {
			return nil, fmt.Errorf("keepalived: register metrics: %w", err)
		}
	}
	return s, nil
}

// Args is keepalived's command line: foreground, log to the console, VRRP
// subsystem only.
func (s *Supervisor) Args() []string {
	return []string{"--dont-fork", "--log-console", "--vrrp", "--use-file", s.cfg.ConfigPath}
}

// candidatePath is keepalived.candidate.conf next to the config.
func (s *Supervisor) candidatePath() string {
	ext := filepath.Ext(s.cfg.ConfigPath)
	return strings.TrimSuffix(s.cfg.ConfigPath, ext) + ".candidate" + ext
}

// install renders c, validates it with `keepalived -t` and swaps it in.
// It reports whether the file changed.
func (s *Supervisor) install(ctx context.Context, c Config) (bool, error) {
	s.mu.Lock()
	old, oldData := s.current, s.data
	s.mu.Unlock()
	if oldData != nil {
		if err := sameVRIDs(old, c); err != nil {
			s.reloads.WithLabelValues("invalid").Inc()
			return false, err
		}
	}
	data, err := Render(c)
	if err != nil {
		s.reloads.WithLabelValues("invalid").Inc()
		return false, err
	}
	if bytes.Equal(data, oldData) {
		s.reloads.WithLabelValues("unchanged").Inc()
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(s.cfg.ConfigPath), 0o755); err != nil { //nolint:gosec // keepalived.conf holds no secrets (keys are file: references)
		return false, err
	}
	if err := os.MkdirAll(c.RunDir, 0o755); err != nil { //nolint:gosec // shared with kudurru and dnsdist
		return false, err
	}
	cand := s.candidatePath()
	if err := edge.WriteFileAtomic(cand, data, 0o644); err != nil { //nolint:gosec // see above
		return false, err
	}
	if out, err := edge.Check(ctx, s.cfg.Binary, "-t", "-f", cand); err != nil {
		s.reloads.WithLabelValues("check_failed").Inc()
		return false, err
	} else if out != "" {
		s.log.Debug("keepalived -t", "output", out)
	}
	if err := edge.SwapFile(cand, s.cfg.ConfigPath); err != nil {
		return false, err
	}
	s.mu.Lock()
	s.current, s.data = c, data
	s.mu.Unlock()
	s.reloads.WithLabelValues("ok").Inc()
	s.log.Info("rendered keepalived configuration", "file", s.cfg.ConfigPath, "vips", len(c.VIPs), "peers", c.Peers, "legacy", c.Legacy)
	if c.Legacy {
		s.log.Warn("legacy mode: keepalived 2.3.x without auth_hmac, VRRP adverts are unauthenticated")
	}
	return true, nil
}

// sameVRIDs refuses a re-render that would change the VRID of a VIP: the
// VRID is hostOnly and never changes underneath a running cluster.
func sameVRIDs(old, next Config) error {
	for _, o := range old.VIPs {
		for _, n := range next.VIPs {
			if o.Name == n.Name && o.VRID != n.VRID {
				return fmt.Errorf("keepalived: VIP %s: VRID %d → %d refused; the VRID is host-only and never changes on re-render (restart `kudurru vip run` with the new value on every node)", o.Name, o.VRID, n.VRID)
			}
		}
	}
	return nil
}

// Rerender installs c and SIGHUPs keepalived when the file changed.
func (s *Supervisor) Rerender(ctx context.Context, c Config) error {
	changed, err := s.install(ctx, c)
	if err != nil || !changed {
		return err
	}
	s.log.Info("reloading keepalived (SIGHUP)")
	return s.Signal(syscall.SIGHUP)
}

// Signal forwards sig to keepalived.
func (s *Supervisor) Signal(sig os.Signal) error {
	s.mu.Lock()
	c := s.child
	s.mu.Unlock()
	if c == nil {
		return nil
	}
	return c.Signal(sig)
}

func (s *Supervisor) startChild() error {
	c := &edge.Child{Path: s.cfg.Binary, Args: s.Args(), Env: s.cfg.ChildEnv, StopTimeout: s.cfg.StopTimeout}
	if err := c.Start(); err != nil {
		return err
	}
	s.mu.Lock()
	s.child = c
	s.mu.Unlock()
	s.log.Info("started keepalived", "pid", c.Pid(), "cmd", s.cfg.Binary+" "+strings.Join(s.Args(), " "))
	return nil
}

// Run installs the configuration, starts keepalived and supervises it
// until ctx ends; keepalived then gets SIGTERM (it releases the VIPs) and
// StopTimeout before SIGKILL.
func (s *Supervisor) Run(ctx context.Context) error {
	c := s.cfg.Config
	if s.cfg.PeersFile != "" {
		peers, err := ReadPeersFile(s.cfg.PeersFile)
		if err != nil {
			return err
		}
		c.Peers = peers
		c.NodeCount = 0
	}
	if _, err := s.install(ctx, c); err != nil {
		return err
	}
	if err := s.startChild(); err != nil {
		return err
	}
	defer func() {
		s.mu.Lock()
		ch := s.child
		s.mu.Unlock()
		ch.Stop()
		s.log.Info("stopped keepalived", "exit", fmt.Sprint(ch.Err()))
	}()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	defer wg.Wait()
	defer cancel()

	peersChanged := make(chan struct{}, 1)
	if s.cfg.PeersFile != "" {
		if err := watchFile(ctx, &wg, s.cfg.PeersFile, peersChanged, s.log); err != nil {
			s.log.Warn("peers file watch disabled", "file", s.cfg.PeersFile, "err", err)
		}
	}
	wg.Go(func() { s.pollState(ctx) })

	backoff := 500 * time.Millisecond
	for {
		s.mu.Lock()
		done := s.child.Done()
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil
		case <-peersChanged:
			peers, err := ReadPeersFile(s.cfg.PeersFile)
			if err != nil {
				s.log.Error("peers file", "err", err)
				continue
			}
			s.mu.Lock()
			next := s.current
			s.mu.Unlock()
			next.Peers, next.NodeCount = peers, 0
			if err := s.Rerender(ctx, next); err != nil {
				s.log.Error("re-render failed; keepalived keeps the previous configuration", "err", err)
			}
		case <-done:
			s.log.Error("keepalived exited unexpectedly; restarting", "exit", fmt.Sprint(s.child.Err()), "backoff", backoff)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, 30*time.Second)
			if err := s.startChild(); err != nil {
				s.log.Error("keepalived restart failed", "err", err)
			}
		}
	}
}

// ReadPeersFile reads one address per line; blank lines and # comments
// are ignored.
func ReadPeersFile(path string) ([]string, error) {
	b, err := os.ReadFile(path) //nolint:gosec // operator-supplied path
	if err != nil {
		return nil, fmt.Errorf("keepalived: peers file: %w", err)
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimSpace(l)
		if i := strings.IndexByte(l, '#'); i >= 0 {
			l = strings.TrimSpace(l[:i])
		}
		if l == "" {
			continue
		}
		for _, f := range strings.FieldsFunc(l, func(r rune) bool { return r == ',' || r == ' ' }) {
			if net.ParseIP(f) == nil {
				return nil, fmt.Errorf("keepalived: peers file %s: %q is not an address", path, f)
			}
			out = append(out, f)
		}
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

func watchFile(ctx context.Context, wg *sync.WaitGroup, path string, ch chan<- struct{}, log *slog.Logger) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	// Watch the directory: ConfigMap volumes replace files by renaming a
	// ..data symlink.
	if err := w.Add(filepath.Dir(path)); err != nil {
		_ = w.Close()
		return err
	}
	wg.Go(func() {
		defer func() { _ = w.Close() }()
		for {
			select {
			case <-ctx.Done():
				return
			case _, ok := <-w.Events:
				if !ok {
					return
				}
				select {
				case ch <- struct{}{}:
				default:
				}
			case err, ok := <-w.Errors:
				if !ok {
					return
				}
				log.Warn("file watch", "err", err)
			}
		}
	})
	return nil
}

// pollState mirrors the FIFO state file (written by `kudurru vip fifo`,
// which keepalived runs as the notify FIFO script) into metrics, logs and
// OnEvent.
func (s *Supervisor) pollState(ctx context.Context) {
	t := time.NewTicker(s.cfg.StatePoll)
	defer t.Stop()
	for {
		s.mu.Lock()
		runDir, node := s.current.RunDir, s.current.RouterID
		names := make([]string, 0, len(s.current.VIPs))
		for _, v := range s.current.VIPs {
			names = append(names, v.Name)
		}
		prev := s.state
		s.mu.Unlock()
		st, err := ReadState(runDir)
		if err != nil {
			s.log.Debug("vip state", "err", err)
		} else {
			for _, name := range names {
				cur := st.VIPs[name]
				v := 0.0
				if cur.State == StateMaster {
					v = 1
				}
				s.master.WithLabelValues(name).Set(v)
				s.priority.WithLabelValues(name).Set(float64(cur.Priority))
				if old := prev.VIPs[name]; old.State != cur.State && cur.State != "" {
					ev := Event{VIP: name, Node: node, State: cur.State, Previous: old.State, Priority: cur.Priority, At: cur.Since}
					s.log.Info("vip state changed", "event", "vip.master_changed", "audit", "cluster.vip.changed",
						"vip", name, "state", cur.State, "previous", old.State, "priority", cur.Priority)
					if s.cfg.OnEvent != nil {
						s.cfg.OnEvent(ev)
					}
				}
			}
			s.mu.Lock()
			s.state = st
			s.mu.Unlock()
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Ready reports whether keepalived runs.
func (s *Supervisor) Ready() (bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.child == nil || !s.child.Running() {
		return false, "keepalived is not running"
	}
	return true, "keepalived running"
}

// Handler serves /healthz/live, /healthz/ready, /status and /metrics.
func (s *Supervisor) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz/live", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /healthz/ready", func(w http.ResponseWriter, _ *http.Request) {
		ok, msg := s.Ready()
		if !ok {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_, _ = w.Write([]byte(msg + "\n"))
	})
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		body := map[string]any{"config": s.cfg.ConfigPath, "state": s.state, "peers": s.current.Peers}
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	})
	mux.Handle("GET /metrics", promhttp.HandlerFor(s.gatherer, promhttp.HandlerOpts{}))
	return mux
}

// Serve runs Handler on addr until ctx ends.
func (s *Supervisor) Serve(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
