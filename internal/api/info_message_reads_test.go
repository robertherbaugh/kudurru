package api

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/central"
	"github.com/robertherbaugh/kudurru/internal/health"
	"github.com/robertherbaugh/kudurru/internal/policy"
	"github.com/robertherbaugh/kudurru/internal/secondary"
	"github.com/robertherbaugh/kudurru/internal/stub"
)

type infoReadFunc func(context.Context, time.Time, func(string, string, string, string, string, string) error) error

func (f infoReadFunc) InfoMessages(ctx context.Context, now time.Time, emit func(string, string, string, string, string, string) error) error {
	return f(ctx, now, emit)
}

type infoPanicLive struct{}

func (infoPanicLive) Index() *policy.Index    { panic("live policy consulted") }
func (infoPanicLive) Ready() (bool, []string) { panic("live lists consulted") }

func TestCapturedInfoMessagesStructuralSourceOneClockAndOperationalMessages(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	calls, clocks := 0, 0
	checker := health.New(nil)
	checker.Set("listener", false, "not bound yet")
	h := newHarness(t, func(o *Options) {
		o.Policy, o.Lists, o.Health = infoPanicLive{}, infoPanicLive{}, checker
		// Uninitialized managers would panic if the captured branch attempted
		// to obtain live configuration/status rather than using the capability.
		o.Secondaries, o.Stubs = &secondary.Manager{}, &stub.Manager{}
		o.Central = centralStub{central.State{Since: now.Add(-time.Hour), LastError: "offline"}}
		o.Now = func() time.Time { clocks++; return now }
		o.InfoMessageReads = infoReadFunc(func(ctx context.Context, observed time.Time, emit func(string, string, string, string, string, string) error) error {
			calls++
			if !observed.Equal(now) || ctx.Err() != nil {
				t.Fatal("reader received wrong context/observation", observed)
			}
			if err := emit("error", "stub", "ServingUnavailable", "DNSZone/stub", "original route unavailable", ""); err != nil {
				return err
			}
			return emit("warning", "policy", "MissingReference", "Policy/apps/original", "original missing reference", "")
		})
	})
	var out List[DiagnosisMessage]
	h.do(http.MethodGet, "/api/v1/info/messages", "viewer", nil).want(http.StatusOK).json(&out)
	if calls != 1 || clocks != 1 || len(out.Items) != 4 {
		t.Fatal("mixed observation clock or omitted observations", calls, clocks, out.Items)
	}
	if !slices.IsSortedFunc(out.Items, func(a, b DiagnosisMessage) int { return strings.Compare(a.Source, b.Source) }) {
		t.Fatal("messages not deterministic", out.Items)
	}
	for _, m := range out.Items {
		if m.Node != "node-a" || m.Source == "health" && !strings.HasPrefix(m.Message, "Current node readiness: ") {
			t.Fatal("operational observation misidentified", m)
		}
	}
	h.do(http.MethodGet, "/api/v1/info/messages", "zoneadmin", nil).want(http.StatusForbidden)
	if calls != 1 {
		t.Fatal("unauthorized request reached reader")
	}
	// Doctor delegates diagnostics with its existing original context.
	var report DoctorReport
	h.do(http.MethodGet, "/api/v1/doctor", "viewer", nil).want(http.StatusOK).json(&report)
	if calls != 2 || !slices.ContainsFunc(report.Checks, func(c DoctorCheck) bool { return c.Check == "diagnoses" && c.Status == DoctorFail }) {
		t.Fatal("doctor bypassed captured diagnostics", report)
	}
}

func TestCapturedInfoMessagesRejectsMissingCapabilitiesAndPartialResults(t *testing.T) {
	for _, mode := range []string{"objects", "zones", "settings", "producer-error", "ignored-emit-error", "canceled", "zero-clock"} {
		t.Run(mode, func(t *testing.T) {
			h := newHarness(t)
			h.srv.opts.Policy, h.srv.opts.Lists = infoPanicLive{}, infoPanicLive{}
			switch mode {
			case "objects":
				h.srv.opts.ObjectReads = &objectReadFixture{}
			case "zones":
				h.srv.opts.ZoneReads = &zoneReadFixture{}
			case "settings":
				h.srv.opts.CanonicalSettings = policyReadSettings{}
			default:
				h.srv.opts.InfoMessageReads = infoReadFunc(func(_ context.Context, _ time.Time, emit func(string, string, string, string, string, string) error) error {
					if err := emit("warning", "policy", "Original", "Policy/original", "must not leak as partial success", ""); err != nil {
						return err
					}
					if mode == "ignored-emit-error" {
						_ = emit("bad", "policy", "Invalid", "", "", "")
						return nil
					}
					return errors.New("original owner unavailable")
				})
			}
			if mode == "zero-clock" {
				h.srv.opts.Now = func() time.Time { return time.Time{} }
			}
			req := h.req(http.MethodGet, "/api/v1/info/messages", "viewer", nil)
			if mode == "canceled" {
				ctx, cancel := context.WithCancel(req.Context())
				cancel()
				req = req.WithContext(ctx)
			}
			h.serve(req).want(http.StatusServiceUnavailable)
		})
	}
}

func TestInfoMessageCollectorBoundsAndStickyAbort(t *testing.T) {
	valid := DiagnosisMessage{Severity: "warning", Source: "policy", Reason: "MissingReference", Message: "missing"}
	for _, field := range []string{"severity", "source", "reason", "object", "message", "hint", "node", "nul", "utf8"} {
		t.Run(field, func(t *testing.T) {
			c := infoMessageCollector{node: "node-a"}
			m := valid
			switch field {
			case "severity":
				m.Severity = "fatal"
			case "source":
				m.Source = strings.Repeat("s", 257)
			case "reason":
				m.Reason = strings.Repeat("r", 257)
			case "object":
				m.Object = strings.Repeat("o", 513)
			case "message":
				m.Message = strings.Repeat("m", maxInfoMessageText+1)
			case "hint":
				m.Hint = strings.Repeat("h", (8<<10)+1)
			case "node":
				c.node = strings.Repeat("n", 257)
			case "nul":
				m.Message = "bad\x00message"
			case "utf8":
				m.Message = string([]byte{0xff})
			}
			err := c.add(m)
			if err == nil || c.add(valid) != err || len(c.items) != 0 || c.bytes != 0 {
				t.Fatal("invalid message allocated output or ignored abort", c, err)
			}
		})
	}
	c := infoMessageCollector{node: "node-a"}
	for range maxInfoMessages {
		if err := c.add(valid); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.add(valid); err == nil || len(c.items) != maxInfoMessages {
		t.Fatal("count cap bypassed", err)
	}
	c = infoMessageCollector{node: "node-a"}
	large := valid
	large.Message = strings.Repeat("x", maxInfoMessageText)
	accepted := 0
	for c.add(large) == nil {
		accepted++
	}
	if c.bytes > maxInfoMessageBytes || accepted == 0 || len(c.items) != accepted || c.bytes+len(large.Message) <= maxInfoMessageBytes {
		t.Fatal("aggregate byte cap", c.bytes, len(c.items))
	}
	// A message ending exactly on the byte boundary is valid; the next byte is not.
	c = infoMessageCollector{bytes: maxInfoMessageBytes - 4}
	if err := c.add(DiagnosisMessage{Severity: "info"}); err != nil || c.bytes != maxInfoMessageBytes {
		t.Fatal("exact byte boundary", c.bytes, err)
	}
	if c.add(DiagnosisMessage{Severity: "info"}) == nil {
		t.Fatal("past exact byte boundary accepted")
	}
}

func TestCapturedInfoMessagesEmptyLegacyAndOperationalBounds(t *testing.T) {
	h := newHarness(t)
	var out List[DiagnosisMessage]
	h.do(http.MethodGet, "/api/v1/info/messages", "viewer", nil).want(http.StatusOK).json(&out)
	h.srv.opts.InfoMessageReads = infoReadFunc(func(context.Context, time.Time, func(string, string, string, string, string, string) error) error {
		return nil
	})
	h.do(http.MethodGet, "/api/v1/info/messages", "viewer", nil).want(http.StatusOK).json(&out)
	if out.Items == nil || len(out.Items) != 0 {
		t.Fatal("empty list must be nonnil", out)
	}
	ctx, cancel := context.WithCancel(t.Context())
	h.srv.opts.Health = health.New(nil)
	h.srv.opts.Health.Register("cancels", func() (bool, []string) { cancel(); return true, nil })
	if _, err := h.srv.infoMessages(ctx, nil); err == nil {
		t.Fatal("operational work hid request cancellation")
	}
	h.srv.opts.Health = nil
	h.srv.opts.Central = centralStub{central.State{Since: time.Time{}, LastError: strings.Repeat("x", maxInfoMessageText)}}
	h.do(http.MethodGet, "/api/v1/info/messages", "viewer", nil).want(http.StatusServiceUnavailable)
	h.srv.opts.Central = nil
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bad.yaml"), []byte("kind: DNSZone\nspec: {type: nope}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	l := loaderFor(t, h.st, dir)
	l.Load(t.Context())
	h.srv.opts.Objects = l
	h.do(http.MethodGet, "/api/v1/info/messages", "viewer", nil).want(http.StatusOK).json(&out)
	if len(out.Items) != 1 || out.Items[0].Source != "objects" || !strings.HasPrefix(out.Items[0].Message, "Current object loader: ") {
		t.Fatal("loader observation hidden or mislabeled", out)
	}
}
