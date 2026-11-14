package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/ee/edge"
	"github.com/robertherbaugh/kudurru/ee/edge/keepalived"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/api/apitest"
	"github.com/robertherbaugh/kudurru/internal/audit"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
)

func notificationFrame(reader *bufio.Reader) (string, api.VIPStreamEvent, error) {
	var name string
	var event api.VIPStreamEvent
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return "", event, err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			return name, event, nil
		}
		if value, ok := strings.CutPrefix(line, "event: "); ok {
			name = value
		}
		if value, ok := strings.CutPrefix(line, "data: "); ok {
			if err := json.Unmarshal([]byte(value), &event); err != nil {
				return "", event, err
			}
		}
	}
}

func TestVIPNotificationsActualFIFOAuditAndMountedRouter(t *testing.T) {
	cfg, err := config.Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Node.Name = "node-a"
	cfg.VIP.Observation.Provider = "keepalived"
	cfg.VIP.Observation.RunDir = t.TempDir()
	cfg.VIP.Observation.Interval = config.Duration(5 * time.Millisecond)
	cfg.VIP.VIPs = []config.VIPInstance{{Name: "dns", Address: "192.0.2.53/24", Interface: "eth0", VRID: 53}}
	fifo := filepath.Join(cfg.VIP.Observation.RunDir, "notify.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	consumer := &keepalived.Consumer{RunDir: cfg.VIP.Observation.RunDir, Node: "node-a", Log: slog.New(slog.DiscardHandler)}
	ctx, cancel := context.WithCancel(t.Context())
	consumerDone := make(chan error, 1)
	go func() { consumerDone <- consumer.ConsumeFIFO(ctx, fifo) }()
	defer func() {
		cancel()
		if err := <-consumerDone; err != nil {
			t.Error("FIFO Consumer failed to join")
		}
	}()
	var writer *os.File
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		writer, err = os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0) //nolint:gosec // FIFO fixture is inside t.TempDir, never external input
		if err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if writer == nil {
		t.Fatal("actual FIFO Consumer did not open source")
	}
	defer func() { _ = writer.Close() }()
	recorder := &audit.Memory{}
	hub, run, err := keepalivedVIPObservation(cfg, recorder, slog.New(slog.DiscardHandler))
	if err != nil || hub == nil || run == nil {
		t.Fatal("production observation mount failed")
	}
	done := make(chan error, 1)
	go func() { done <- run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Error("mounted observer did not join")
		}
	}()
	deadline = time.Now().Add(3 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		rows, err := recorder.Read(ctx, audit.Query{})
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			if row.Reason == "source_ready" {
				ready = true
			}
		}
		if ready {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !ready {
		t.Fatal("mounted source did not establish baseline")
	}
	st := memory.New()
	defer func() { _ = st.Close() }()
	v, err := validator.New()
	if err != nil {
		t.Fatal(err)
	}
	router, err := api.New(api.Options{Store: st, Validator: v, Config: cfg, VIPEvents: hub, Authenticate: apitest.Authenticate, Authorizer: apitest.Authorizer(apitest.StandardBindings()...), Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	defer router.Close()
	server := httptest.NewServer(router)
	defer server.Close()
	requestCtx, requestCancel := context.WithTimeout(ctx, 5*time.Second)
	defer requestCancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, server.URL+"/api/v1/edge/vip/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", apitest.Bearer("admin"))
	response, err := server.Client().Do(request)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatal("mounted stream could not open")
	}
	defer func() { _ = response.Body.Close() }()
	reader := bufio.NewReader(response.Body)
	if _, _, err := notificationFrame(reader); err != nil {
		t.Fatal("stream connection frame missing")
	}
	if _, err := fmt.Fprintln(writer, "INSTANCE \"VI_dns\" BACKUP 180\nINSTANCE \"VI_dns\" MASTER 200\nINSTANCE \"VI_dns\" FAULT 0"); err != nil {
		t.Fatal("could not write notification fixture")
	}
	for i, want := range []string{"BACKUP", "MASTER", "FAULT"} {
		name, event, err := notificationFrame(reader)
		previous := ""
		switch i {
		case 1:
			previous = "BACKUP"
		case 2:
			previous = "MASTER"
		}
		if err != nil || name != "vip.master_changed" || event.State != want || event.Previous != previous || event.Node != "node-a" || event.VRID != 53 || event.Sequence != uint64(i+1) || event.Session == "" {
			t.Fatal("FIFO-to-audit/SSE transition lost original identity")
		}
	}
	rows, err := recorder.Read(ctx, audit.Query{Kind: "vip"})
	if err != nil || len(rows) != 3 {
		t.Fatal("observed transitions not audited exactly once")
	}
	for i, row := range rows {
		if row.Action != "cluster.vip.changed" || row.Authz != "host-config" || row.Actor.Name != "keepalived-observer" {
			t.Fatal("runtime event borrowed request/writer authority")
		}
		var event api.VIPStreamEvent
		if json.Unmarshal(row.After, &event) != nil || event.VRID != 53 || event.Node != "node-a" || event.VIP != "dns" || event.Sequence != uint64(i+1) || !event.ExpiresAt.Equal(event.At.Add(cfg.VIP.Observation.MaxAge.Std())) {
			t.Fatal("audit lost original node/VRID/sequence/time evidence")
		}
	}
	if _, err := os.Stat(filepath.Join(cfg.VIP.Observation.RunDir, "weight-dns")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("observation accessed weight writer")
	}
	// A refused source interrupts the old stream AND refuses new requests.
	// Freshly reopening a stream must not conceal the acquisition failure.
	foreign, err := keepalived.ReadState(cfg.VIP.Observation.RunDir)
	if err != nil {
		t.Fatal(err)
	}
	foreign.Node = "foreign"
	wire, err := json.Marshal(foreign)
	if err != nil {
		t.Fatal(err)
	}
	if err := edge.WriteFileAtomic(keepalived.StatePath(cfg.VIP.Observation.RunDir), wire, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := notificationFrame(reader); err == nil {
		t.Fatal("foreign node source retained active subscription")
	}
	retry, err := http.NewRequestWithContext(requestCtx, http.MethodGet, server.URL+"/api/v1/edge/vip/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	retry.Header.Set("Authorization", apitest.Bearer("admin"))
	refused, err := server.Client().Do(retry)
	if err != nil {
		t.Fatal("source refusal qualification request failed")
	}
	_ = refused.Body.Close()
	if refused.StatusCode != http.StatusServiceUnavailable {
		t.Fatal("new stream concealed refused source")
	}
}

func TestVIPNotificationStreamOriginalServingCredentialExpiry(t *testing.T) {
	f := newServingCommandFixture(t)
	hub := &api.VIPEventHub{}
	router, err := api.New(api.Options{Store: f.s.opts.Store, Validator: f.s.opts.Validator, Config: f.s.opts.Config, Authenticate: f.http.Middleware, Authorizer: f.http, VIPEvents: hub, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	defer router.Close()
	server := httptest.NewServer(router)
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/v1/edge/vip/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer "+f.token)
	response, err := server.Client().Do(r)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatal("actual stored credential failed initial stream authorization")
	}
	defer func() { _ = response.Body.Close() }()
	reader := bufio.NewReader(response.Body)
	if _, _, err := notificationFrame(reader); err != nil {
		t.Fatal("stored credential stream did not connect")
	}
	hub.Publish(api.VIPStreamEvent{Node: "node-a", VIP: "dns", VRID: 53, State: "MASTER", At: time.Now(), ExpiresAt: time.Now().Add(time.Minute)})
	if name, _, err := notificationFrame(reader); err != nil || name != "vip.master_changed" {
		t.Fatal("live original credential failed event authorization")
	}
	f.offset.Store(int64(time.Hour))
	hub.Publish(api.VIPStreamEvent{Node: "node-a", VIP: "dns", VRID: 53, State: "FAULT", At: time.Now(), ExpiresAt: time.Now().Add(time.Minute)})
	if _, _, err := notificationFrame(reader); err == nil {
		t.Fatal("expired original stored credential received later event")
	}
}

type notificationAuditFunc func(context.Context, audit.Event) error

func (f notificationAuditFunc) Record(ctx context.Context, event audit.Event) error {
	return f(ctx, event)
}

func TestVIPNotificationOriginalAuditDeadlineAndRefusal(t *testing.T) {
	cfg, err := config.Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Node.Name = "node-a"
	cfg.VIP.VIPs = []config.VIPInstance{{Name: "dns", Address: "192.0.2.53/24", Interface: "eth0", VRID: 53}}
	cfg.VIP.Observation.Provider = "keepalived"
	cfg.VIP.Observation.RunDir = t.TempDir()
	cfg.VIP.Observation.Interval = config.Duration(5 * time.Millisecond)
	cfg.VIP.Observation.Timeout = config.Duration(100 * time.Millisecond)
	c := &keepalived.Consumer{RunDir: cfg.VIP.Observation.RunDir, Node: "node-a", Log: slog.New(slog.DiscardHandler)}
	if err := c.Reset(); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	refused := make(chan bool, 1)
	recorder := notificationAuditFunc(func(ctx context.Context, _ audit.Event) error {
		calls.Add(1)
		deadline, ok := ctx.Deadline()
		bounded := ok && time.Until(deadline) <= 100*time.Millisecond
		<-ctx.Done()
		refused <- bounded
		return ctx.Err()
	})
	hub, run, err := keepalivedVIPObservation(cfg, recorder, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	cfg.VIP.Observation.Timeout = config.Duration(5 * time.Second)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Error("observer did not join after audit refusal")
		}
	}()
	select {
	case bounded := <-refused:
		if !bounded {
			t.Fatal("later config repaired original audit deadline")
		}
	case <-time.After(time.Second):
		t.Fatal("audit acquisition exceeded original deadline")
	}
	if _, release, err := hub.Subscribe(t.Context()); err == nil {
		release()
		t.Fatal("failed audit acquisition admitted notification stream")
	}
	if calls.Load() != 1 {
		t.Fatal("refused audit was retried")
	}
}
