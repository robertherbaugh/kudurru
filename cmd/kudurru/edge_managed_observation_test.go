package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/robertherbaugh/kudurru/ee/edge"
	"github.com/robertherbaugh/kudurru/ee/edge/dnsdist"
	"github.com/robertherbaugh/kudurru/internal/cluster"
	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/license"
)

func TestEdgeManagedObservationStartupRequirements(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sup, err := dnsdist.New(dnsdist.Config{Options: edge.Options{ManagedCacheBypass: true}, Source: &cluster.StaticSource{Endpoints: []cluster.Endpoint{{Address: "127.0.0.1:5353", Ready: true, Serving: true}}}, ConfigDir: t.TempDir(), Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	creds, err := internalapi.NewCredentials(internalapi.CredentialOptions{Key: bytes.Repeat([]byte{7}, 32), NodeName: "edge-b"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name        string
		flags       edgeFlags
		credentials *internalapi.Credentials
		want        bool
	}{
		{"disabled", edgeFlags{}, nil, true},
		{"missing-bypass", edgeFlags{managedObservationAddr: "127.0.0.1:8085"}, creds, false},
		{"missing-credentials", edgeFlags{managedObservationAddr: "127.0.0.1:8085", managedCacheBypass: true}, nil, false},
		{"bad-address", edgeFlags{managedObservationAddr: "https://peer/path", managedCacheBypass: true}, creds, false},
		{"zero-port", edgeFlags{managedObservationAddr: "127.0.0.1:0", managedCacheBypass: true}, creds, false},
		{"configured", edgeFlags{managedObservationAddr: "127.0.0.1:8085", managedCacheBypass: true}, creds, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, err := tc.flags.managedObservationServer(sup, log, tc.credentials)
			if (err == nil) != tc.want || tc.name == "disabled" && srv != nil {
				t.Fatal("observation listener bypassed startup requirements", err)
			}
		})
	}
	setting, ok := config.Lookup("edge.managedObservationAddress")
	if !ok || !setting.HostOnly || !setting.RestartRequired || setting.Default != "" {
		t.Fatal("observation listener is not an explicit host-only startup setting")
	}
	cmd := &cobra.Command{Use: "run"}
	f := &edgeFlags{}
	f.register(cmd, true)
	path := writeTemp(t, "edge: {managedCacheBypass: true, managedObservationAddress: '127.0.0.1:8085'}\n")
	if err := cmd.ParseFlags([]string{"--managed-observation-address=127.0.0.1:8086"}); err != nil {
		t.Fatal(err)
	}
	if err := f.load(cmd, path); err != nil || f.managedObservationAddr != "127.0.0.1:8086" || !f.managedCacheBypass {
		t.Fatal("explicit listener flag did not preserve typed host configuration", err)
	}
}

func TestEdgeRunObservationBindFailureJoinsSupervisor(t *testing.T) {
	key := trustTestKey(t)(license.Claims{Plan: license.PlanPersonal, Nodes: 3, Features: []license.Feature{license.FeatureEdge}})
	t.Setenv(license.EnvKey, key)
	t.Setenv(license.EnvKeyFile, "")
	t.Setenv("KUDURRU_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
	t.Setenv("KUDURRU_ENCRYPTION_KEY_FILE", "")
	t.Setenv("KUDURRU_ENCRYPTION_KEY_PREVIOUS", "")
	t.Setenv("KUDURRU_ENCRYPTION_KEY_PREVIOUS_FILE", "")
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = occupied.Close() }()
	cmd := &cobra.Command{Use: "run"}
	f := &edgeFlags{}
	f.register(cmd, true)
	f.backends = "node-a=127.0.0.1:5353"
	f.pollZones = false
	f.listen = []string{"127.0.0.1"}
	f.managedCacheBypass = true
	f.managedObservationAddr = occupied.Addr().String()
	f.healthAddr = offlineFreeAddress(t, "tcp")
	f.configDir = filepath.Join(t.TempDir(), "config")
	f.dnsdistBin = filepath.Join(t.TempDir(), "child")
	// Lifecycle-only child; no cache behavior is inferred from this script.
	if err := os.WriteFile(f.dnsdistBin, []byte("#!/bin/sh\nif [ \"$1\" = --check-config ]; then exit 0; fi\nexec sleep 30\n"), 0o700); err != nil { //nolint:gosec // executable owned lifecycle fixture
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	cmd.SetContext(ctx)
	err = runEdgeRun(cmd, f)
	if err == nil || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil || !strings.Contains(err.Error(), "listen") {
		t.Fatal("failed observation listener did not stop and join supervisor", err)
	}
	// The health listener has also joined; its original address is reusable.
	reopened, err := net.Listen("tcp", f.healthAddr)
	if err != nil {
		t.Fatal("failed startup leaked health listener", err)
	}
	_ = reopened.Close()
}
