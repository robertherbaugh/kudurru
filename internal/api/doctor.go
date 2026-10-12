package api

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/robertherbaugh/kudurru/internal/auth"
)

// Doctor check statuses.
const (
	DoctorOK   = "ok"
	DoctorWarn = "warn"
	DoctorFail = "fail"
)

// DoctorCheck is one line of GET /doctor, named like the `kudurru doctor`
// checks.
type DoctorCheck struct {
	Check   string `json:"check"`
	Status  string `json:"status" enum:"ok,warn,fail"`
	Message string `json:"message"`
}

// DoctorReport is GET /doctor: the server-side half of `kudurru doctor`
// (config, api, token, diagnoses, dataDir, ready) as this node sees it. The
// listener ports are covered by the readiness of the dns component; the
// CLI additionally probes ports from the host.
type DoctorReport struct {
	Node     string        `json:"node"`
	Version  string        `json:"version"`
	Checks   []DoctorCheck `json:"checks"`
	Failures int           `json:"failures"`
	Warnings int           `json:"warnings"`
	Took
}

type doctorOutput struct{ Body *DoctorReport }

func (s *Server) registerDoctor() {
	register(s, operation(http.MethodGet, "/doctor", "doctor", "The checks of `kudurru doctor` as this node sees them", "cluster"),
		perm{resource: "settings", verb: "read"}, s.doctor)
}

func (s *Server) doctor(ctx context.Context, _ *struct{}) (*doctorOutput, error) {
	rep := &DoctorReport{Node: s.opts.NodeName, Version: s.opts.Version.Version, Checks: []DoctorCheck{}}
	add := func(check, status, format string, args ...any) {
		rep.Checks = append(rep.Checks, DoctorCheck{Check: check, Status: status, Message: fmt.Sprintf(format, args...)})
		switch status {
		case DoctorFail:
			rep.Failures++
		case DoctorWarn:
			rep.Warnings++
		}
	}
	cfg := s.config()
	src := cfg.Source
	if src == "" {
		src = "built-in defaults"
	}
	if cfg.SettingsFile != "" {
		src += ", settings " + cfg.SettingsFile
	}
	add("config", DoctorOK, "%s", src)
	add("api", DoctorOK, "kudurru %s answers on node %s", s.opts.Version.Version, s.opts.NodeName)

	if p, ok := auth.PrincipalFrom(ctx); ok {
		var roles []string
		if s.opts.Roles != nil {
			roles = s.opts.Roles(ctx, p)
		}
		name := p.Name
		if name == "" {
			name = p.ID
		}
		add("token", DoctorOK, "%s %s, roles %s", p.Kind, name, orNone(strings.Join(roles, ",")))
	}

	msgs, err := s.infoMessages(ctx, nil)
	switch {
	case err != nil:
		add("diagnoses", DoctorWarn, "%v", err)
	default:
		var errs, warns int
		for _, m := range msgs.Body.Items {
			switch m.Severity {
			case "error":
				errs++
			case "warning":
				warns++
			}
		}
		switch {
		case errs > 0:
			add("diagnoses", DoctorFail, "%d messages, %d errors (GET /api/v1/info/messages)", len(msgs.Body.Items), errs)
		case warns > 0:
			add("diagnoses", DoctorWarn, "%d messages, %d warnings (GET /api/v1/info/messages)", len(msgs.Body.Items), warns)
		default:
			add("diagnoses", DoctorOK, "%d messages, none a warning", len(msgs.Body.Items))
		}
	}

	status, msg := checkDataDir(cfg.Node.DataDir)
	add("dataDir", status, "%s", msg)

	if ok, unready := s.ready(); ok {
		add("ready", DoctorOK, "every component is ready (listeners bound, zones and lists loaded)")
	} else {
		add("ready", DoctorFail, "not ready: %s", strings.Join(unready, ", "))
	}
	return &doctorOutput{Body: rep}, nil
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// checkDataDir is the dataDir check of `kudurru doctor` run by the server
// process: the directory exists, is writable, is not world-writable and
// its databases are not world-readable.
func checkDataDir(dir string) (status, message string) {
	st, err := os.Stat(dir)
	switch {
	case err != nil:
		return DoctorWarn, err.Error()
	case !st.IsDir():
		return DoctorWarn, dir + " is not a directory"
	}
	mode := st.Mode().Perm()
	f, err := os.CreateTemp(dir, ".kudurru-doctor-*")
	if err != nil {
		return DoctorFail, fmt.Sprintf("%s is not writable by uid %d: %v", dir, os.Getuid(), errors.Unwrap(err))
	}
	_ = f.Close()
	_ = os.Remove(f.Name())
	if mode&0o002 != 0 {
		return DoctorWarn, fmt.Sprintf("%s is world-writable (mode %04o); use 0750 or tighter", dir, mode)
	}
	for _, name := range []string{"state.db", "local.db", "querylog.db"} {
		fst, err := os.Stat(filepath.Join(dir, name))
		switch {
		case err == nil && fst.Mode().Perm()&0o004 != 0:
			return DoctorWarn, fmt.Sprintf("%s is world-readable (mode %04o)", filepath.Join(dir, name), fst.Mode().Perm())
		case err != nil && !errors.Is(err, fs.ErrNotExist):
			return DoctorWarn, err.Error()
		}
	}
	return DoctorOK, fmt.Sprintf("%s (writable, mode %04o)", dir, mode)
}
