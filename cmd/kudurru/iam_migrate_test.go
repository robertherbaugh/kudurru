package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/iammigrate"
	"github.com/spf13/cobra"
)

type migrationFixture struct {
	calls  []string
	rv     string
	ack    bool
	status iammigrate.Status
	err    error
}

func (m *migrationFixture) Check(context.Context) (iammigrate.Status, error) {
	m.calls = append(m.calls, "status")
	return m.status, m.err
}
func (m *migrationFixture) Plan(context.Context) (*iammigrate.Plan, error) {
	m.calls = append(m.calls, "plan")
	return &iammigrate.Plan{}, nil
}
func (m *migrationFixture) Begin(_ context.Context, _ *iammigrate.Plan, ack iammigrate.OfflineAcknowledgement) (iammigrate.Status, error) {
	m.calls = append(m.calls, "begin")
	m.ack = ack.ReadersAndWritersStopped
	return m.status, m.err
}
func (m *migrationFixture) Resume(_ context.Context, rv string, ack iammigrate.OfflineAcknowledgement) (iammigrate.Status, error) {
	m.calls = append(m.calls, "resume")
	m.rv, m.ack = rv, ack.ReadersAndWritersStopped
	return m.status, m.err
}

func TestIAMMigrationCommandRejectsMissingPreconditionsBeforeClients(t *testing.T) {
	for _, args := range [][]string{
		{"status"}, {"begin", "--release-namespace", "dns"},
		{"resume", "--release-namespace", "dns", "--offline"},
		{"status", "--release-namespace", "dns", "--timeout", "0s"},
		{"status", "--release-namespace", "dns", "--timeout", "31m"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			cmd := newIAMMigrationCmd(func(*cobra.Command, iamMigrationOptions) (iamMigrationBackend, error) {
				t.Fatal("constructed Kubernetes clients before checking preconditions")
				return nil, nil
			})
			cmd.SetArgs(args)
			cmd.SetOut(new(bytes.Buffer))
			cmd.SetErr(new(bytes.Buffer))
			if cmd.Execute() == nil {
				t.Fatal("missing precondition accepted")
			}
		})
	}
}

func TestIAMMigrationCommandOperations(t *testing.T) {
	for _, operation := range []string{"status", "plan", "begin", "resume"} {
		t.Run(operation, func(t *testing.T) {
			backend := &migrationFixture{status: iammigrate.Status{State: iammigrate.Complete, RemoteAllowed: true, JournalResourceVersion: "rv/opaque"}}
			cmd := newIAMMigrationCmd(func(c *cobra.Command, opts iamMigrationOptions) (iamMigrationBackend, error) {
				deadline, ok := c.Context().Deadline()
				if !ok || time.Until(deadline) > time.Minute || opts.Namespace != "dns" || opts.Context != "test" || opts.Kubeconfig != "test-config" {
					t.Fatal("client options or bounded context lost")
				}
				return backend, nil
			})
			args := []string{operation, "--release-namespace", "dns", "--kube-context", "test", "--kubeconfig", "test-config", "--timeout", "1m"}
			if operation == "begin" || operation == "resume" {
				args = append(args, "--offline")
			}
			if operation == "resume" {
				args = append(args, "--resource-version", "opaque/stale-exact")
			}
			cmd.SetArgs(args)
			var output bytes.Buffer
			cmd.SetOut(&output)
			cmd.SetErr(&output)
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			want := operation
			if operation == "begin" {
				want = "plan,begin"
			}
			if strings.Join(backend.calls, ",") != want {
				t.Fatal("unexpected migration calls", backend.calls)
			}
			if (operation == "begin" || operation == "resume") && !backend.ack {
				t.Fatal("offline acknowledgment lost")
			}
			if operation == "resume" && backend.rv != "opaque/stale-exact" {
				t.Fatal("resume silently refreshed journal version")
			}
			if operation != "plan" && !strings.Contains(output.String(), `"JournalResourceVersion":"rv/opaque"`) {
				t.Fatal("missing recovery version", output.String())
			}
		})
	}
}

func TestIAMMigrationCommandReportsFailedRecoveryState(t *testing.T) {
	failed := errors.New("migration stopped")
	backend := &migrationFixture{err: failed, status: iammigrate.Status{State: iammigrate.RecoveryRequired, JournalResourceVersion: "trusted-rv"}}
	cmd := newIAMMigrationCmd(func(*cobra.Command, iamMigrationOptions) (iamMigrationBackend, error) { return backend, nil })
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetArgs([]string{"resume", "--release-namespace", "dns", "--offline", "--resource-version", "old"})
	var output bytes.Buffer
	cmd.SetOut(&output)
	if err := cmd.Execute(); !errors.Is(err, failed) {
		t.Fatal("lost failure", err)
	}
	if !strings.Contains(output.String(), "recovery-required") || !strings.Contains(output.String(), "trusted-rv") {
		t.Fatal("missing safe recovery state", output.String())
	}
}
