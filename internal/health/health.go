// Package health is the ops listener: startup, readiness and liveness
// probes with the semantics of docs/architecture.md §5.2. Upstream
// reachability is never a readiness condition.
package health

import (
	"encoding/json"
	"net/http"
	"net/netip"
	"sort"
	"sync"
	"time"
)

// Component is one readiness contributor.
type Component struct {
	Ready   bool      `json:"ready"`
	Message string    `json:"message,omitempty"`
	Since   time.Time `json:"since"`
}

// ReadyFunc is a live readiness source (e.g. the DNS listener registry).
type ReadyFunc func() (ok bool, unready []string)

// Checker aggregates component readiness and serves the probes.
type Checker struct {
	mu         sync.RWMutex
	components map[string]Component
	funcs      map[string]ReadyFunc
	started    bool
	lameduck   bool
	lastLive   time.Time
	info       map[string]any
}

// New returns an empty checker.
func New(info map[string]any) *Checker {
	return &Checker{components: map[string]Component{}, funcs: map[string]ReadyFunc{}, lastLive: time.Now(), info: info}
}

// Register adds a live readiness source evaluated on every probe.
func (c *Checker) Register(name string, fn ReadyFunc) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.funcs[name] = fn
}

// Set records a component's readiness.
func (c *Checker) Set(name string, ready bool, msg string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	prev, ok := c.components[name]
	since := prev.Since
	if !ok || prev.Ready != ready {
		since = time.Now()
	}
	c.components[name] = Component{Ready: ready, Message: msg, Since: since}
}

// SetStarted marks the startup probe satisfied.
func (c *Checker) SetStarted() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.started = true
}

// Lameduck makes readiness fail while the process drains.
func (c *Checker) Lameduck() { c.SetLameduck(true) }

// SetLameduck enters or leaves lameduck (POST/DELETE /lameduck on the ops
// listener; `kudurru lameduck`).
func (c *Checker) SetLameduck(on bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lameduck = on
}

// IsLameduck reports whether the node is draining; the ingress stage then
// answers the health name SERVFAIL so dnsdist drains the node (D51).
func (c *Checker) IsLameduck() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lameduck
}

// Tick records that the self-check loop ran; liveness fails if it stops.
func (c *Checker) Tick() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastLive = time.Now()
}

// Ready reports overall readiness and the names of unready components.
func (c *Checker) Ready() (bool, []string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.lameduck {
		return false, []string{"lameduck"}
	}
	var bad []string
	for name, comp := range c.components {
		if !comp.Ready {
			bad = append(bad, name)
		}
	}
	for name, fn := range c.funcs {
		if ok, unready := fn(); !ok {
			bad = append(bad, name+"("+join(unready)+")")
		}
	}
	sort.Strings(bad)
	return len(bad) == 0, bad
}

// PathLameduck is the ops-listener route of `kudurru lameduck`: POST
// enters lameduck, DELETE leaves it, GET reports it. Only loopback peers
// may change it (the ops listener has no authentication; a preStop hook or
// `docker exec` runs on the node itself).
const PathLameduck = "/lameduck"

// Handler serves /healthz, /healthz/startup, /healthz/ready, /healthz/live
// and /lameduck.
func (c *Checker) Handler() http.Handler {
	mux := http.NewServeMux()
	lameduck := func(w http.ResponseWriter, on bool) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]bool{"lameduck": on})
	}
	mux.HandleFunc("GET "+PathLameduck, func(w http.ResponseWriter, _ *http.Request) { lameduck(w, c.IsLameduck()) })
	set := func(on bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !loopbackPeer(r) {
				http.Error(w, "lameduck can only be changed from the node itself (loopback)", http.StatusForbidden)
				return
			}
			c.SetLameduck(on)
			lameduck(w, on)
		}
	}
	mux.HandleFunc("POST "+PathLameduck, set(true))
	mux.HandleFunc("DELETE "+PathLameduck, set(false))
	mux.HandleFunc("GET /healthz/startup", func(w http.ResponseWriter, _ *http.Request) {
		c.mu.RLock()
		started := c.started
		c.mu.RUnlock()
		status(w, started, "started", "starting")
	})
	mux.HandleFunc("GET /healthz/ready", func(w http.ResponseWriter, _ *http.Request) {
		ok, bad := c.Ready()
		msg := "ready"
		if !ok {
			msg = "not ready: " + join(bad)
		}
		status(w, ok, msg, msg)
	})
	mux.HandleFunc("GET /healthz/live", func(w http.ResponseWriter, _ *http.Request) {
		c.mu.RLock()
		alive := time.Since(c.lastLive) < 30*time.Second
		c.mu.RUnlock()
		status(w, alive, "alive", "self-check loop stalled")
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		ok, bad := c.Ready()
		c.mu.RLock()
		components := make(map[string]Component, len(c.components)+len(c.funcs))
		for name, comp := range c.components {
			components[name] = comp
		}
		for name, fn := range c.funcs {
			fok, unready := fn()
			components[name] = Component{Ready: fok, Message: join(unready), Since: c.lastLive}
		}
		body := map[string]any{"status": map[bool]string{true: "ready", false: "not-ready"}[ok], "unready": bad, "components": components}
		for k, v := range c.info {
			body[k] = v
		}
		c.mu.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		if !ok {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(w).Encode(body)
	})
	return mux
}

// loopbackPeer reports whether the request came from a loopback address.
func loopbackPeer(r *http.Request) bool {
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	return err == nil && ap.Addr().Unmap().IsLoopback()
}

func status(w http.ResponseWriter, ok bool, okMsg, failMsg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if ok {
		_, _ = w.Write([]byte(okMsg + "\n"))
		return
	}
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte(failMsg + "\n"))
}

func join(s []string) string {
	out := ""
	for i, x := range s {
		if i > 0 {
			out += ", "
		}
		out += x
	}
	return out
}
