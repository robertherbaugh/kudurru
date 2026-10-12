package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/central"
	"github.com/robertherbaugh/kudurru/internal/central/centraltest"
	"github.com/robertherbaugh/kudurru/internal/config"
)

func TestDoctorDatabase(t *testing.T) {
	run := func(dsn string) (*doctorReport, string) {
		var out strings.Builder
		rep := &doctorReport{say: func(f string, a ...any) { fmt.Fprintf(&out, f, a...) }}
		cfg := &config.Config{Database: config.Database{DSN: dsn}}
		checkDatabase(context.Background(), rep, cfg)
		return rep, out.String()
	}
	if rep, out := run(""); rep.failures+rep.warnings != 0 || out != "" {
		t.Fatalf("no database: %q", out)
	}
	if rep, out := run("postgres://kudurru:pw@127.0.0.1:1/kudurru?sslmode=disable&connect_timeout=1"); rep.failures != 1 || strings.Contains(out, "pw") {
		t.Fatalf("unreachable: %q", out)
	}
	if rep, out := run("postgres://kudurru@db.example:5432/kudurru?sslmode=disable&connect_timeout=1"); rep.warnings != 1 || !strings.Contains(out, "not encrypted") {
		t.Fatalf("plain text to a remote host: %q", out)
	}
	db := centraltest.DSN(t)
	if rep, out := run(db); rep.failures != 0 || !strings.Contains(out, "no kudurru schema yet") {
		t.Fatalf("fresh database: %q", out)
	}
	centraltest.Open(t, db)
	if rep, out := run(db); rep.failures != 0 || !strings.Contains(out, fmt.Sprintf("schema %d", central.SchemaVersion)) {
		t.Fatalf("migrated database: %q", out)
	}
}
