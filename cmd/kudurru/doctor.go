package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/spf13/cobra"

	"github.com/robertherbaugh/kudurru/ee/edge/keepalived"
	"github.com/robertherbaugh/kudurru/internal/apiclient"
	"github.com/robertherbaugh/kudurru/internal/central"
	"github.com/robertherbaugh/kudurru/internal/cli"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/version"
)

// doctorReport counts the outcome of each check.
type doctorReport struct {
	say      func(format string, args ...any)
	failures int
	warnings int
}

func (r *doctorReport) ok(check, format string, args ...any) {
	r.say("%s: OK %s\n", check, fmt.Sprintf(format, args...))
}

func (r *doctorReport) warn(check, format string, args ...any) {
	r.warnings++
	r.say("%s: WARN %s\n", check, fmt.Sprintf(format, args...))
}

func (r *doctorReport) fail(check, format string, args ...any) {
	r.failures++
	r.say("%s: FAIL %s\n", check, fmt.Sprintf(format, args...))
}

func newDoctorCmd(configPath *string, o *cliOpts) *cobra.Command {
	var (
		skipAPI, skipPorts bool
		vipRunDir, kaConf  string
	)
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check the local configuration, environment and the API server",
		Long: `Check the local configuration and environment, then the API server:

  config     the config file parses and validates
  dataDir    exists, is a directory, is writable, is not world-writable
  ports      every configured listener address can be bound (or is in use by
             a running kudurru)
  api        the API server answers (the CLI context, --server, or the local
             listeners.api.address)
  token      the CLI token is accepted (GET /auth/me)
  diagnoses  configuration diagnoses reported by the server (GET /info/messages)
  database   with database.dsn set: the central database answers, runs
             PostgreSQL 15 or newer and has a schema this binary can use;
             warns about sslmode=disable to another host
  vrrp       warns when ` + "`kudurru vip run --legacy`" + ` renders keepalived without
             auth_hmac (unauthenticated VRRP adverts); silent on nodes
             without keepalived

Exits non-zero when a check fails.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			w := c.OutOrStdout()
			info := version.Get()
			rep := &doctorReport{say: func(format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }}
			say := rep.say
			say("kudurru %s (%s)\n", info.Version, info.Platform)
			cfg, err := loadConfig(c, *configPath)
			if err != nil {
				say("config: FAIL %v\n", err)
				return err
			}
			src := cfg.Source
			if src == "" {
				src = "built-in defaults (no file at " + *configPath + ")"
			}
			say("config: OK (%s)\n", src)
			say("node: %s  dataDir: %s\n", cfg.Node.Name, cfg.Node.DataDir)
			for _, l := range cfg.Listeners.DNS {
				say("dns listener: %s %v\n", l.Address, l.Protocols)
			}
			for name, g := range cfg.Upstreams.Groups {
				say("upstream group %s: %v (%s, %s)\n", name, g.Servers, g.Strategy, g.Timeout)
			}
			if len(cfg.EnvLocked) > 0 {
				say("env-locked keys: %v\n", cfg.EnvLocked)
			}
			checkDataDir(rep, cfg.Node.DataDir)
			checkDatabase(c.Context(), rep, cfg)
			checkVRRP(rep, vipRunDir, kaConf)

			var serverUp bool
			if !skipAPI {
				serverUp = o.checkAPI(c.Context(), rep, cfg)
			}
			if !skipPorts {
				checkPorts(rep, cfg, serverUp)
			}
			say("summary: %d failed, %d warnings\n", rep.failures, rep.warnings)
			if rep.failures > 0 {
				return fmt.Errorf("doctor: %d checks failed", rep.failures)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&skipAPI, "skip-api", false, "do not contact the API server")
	cmd.Flags().BoolVar(&skipPorts, "skip-ports", false, "do not test whether listener ports can be bound")
	cmd.Flags().StringVar(&vipRunDir, "vip-run-dir", keepalived.DefaultRunDir, "run directory shared with `kudurru vip run` (legacy-mode marker)")
	cmd.Flags().StringVar(&kaConf, "keepalived-config", "/etc/keepalived/keepalived.conf", "keepalived.conf rendered by `kudurru vip run`")
	return cmd
}

// checkVRRP warns when keepalived runs in legacy mode (`kudurru vip run
// --legacy`, keepalived 2.3.x): VRRP adverts without auth_hmac let anyone
// on the segment take the VIP. The supervisor leaves a marker in the
// shared run directory; a readable keepalived.conf is checked directly.
func checkVRRP(rep *doctorReport, runDir, confPath string) {
	const hint = "upgrade keepalived to 2.4+ and restart `kudurru vip run` without --legacy (§3.6)"
	if _, err := os.Stat(keepalived.LegacyMarkerPath(runDir)); err == nil {
		rep.warn("vrrp", "`kudurru vip run --legacy` is in use: VRRP adverts are unauthenticated; %s", hint)
		return
	}
	b, err := os.ReadFile(confPath) //nolint:gosec // G304: the operator names the file
	if err != nil {
		return // no keepalived on this node (or not readable from here)
	}
	unauth, total := keepalived.UnauthenticatedInstances(b)
	switch {
	case total == 0:
	case len(unauth) > 0:
		rep.warn("vrrp", "%s: %d of %d VRRP instances without auth_hmac (%s), adverts are unauthenticated; %s",
			confPath, len(unauth), total, strings.Join(unauth, ", "), hint)
	default:
		rep.ok("vrrp", "%d VRRP instances authenticated with auth_hmac", total)
	}
}

// checkDataDir verifies the data directory exists, is writable and is not
// open to everyone.
func checkDataDir(rep *doctorReport, dir string) {
	st, err := os.Stat(dir)
	switch {
	case err != nil:
		rep.warn("dataDir", "%v", err)
		return
	case !st.IsDir():
		rep.warn("dataDir", "%s is not a directory", dir)
		return
	}
	mode := st.Mode().Perm()
	f, err := os.CreateTemp(dir, ".kudurru-doctor-*")
	if err != nil {
		rep.warn("dataDir", "%s is not writable by uid %d: %v (the server needs write access)", dir, os.Getuid(), errors.Unwrap(err))
		return
	}
	_ = f.Close()
	_ = os.Remove(f.Name())
	if mode&0o002 != 0 {
		rep.warn("dataDir", "%s is world-writable (mode %04o); use 0750 or tighter", dir, mode)
		return
	}
	// state.db and local.db hold secrets' hashes and the zone snapshot.
	for _, name := range []string{"state.db", "local.db", "querylog.db"} {
		if fst, err := os.Stat(filepath.Join(dir, name)); err == nil && fst.Mode().Perm()&0o004 != 0 {
			rep.warn("dataDir", "%s is world-readable (mode %04o)", filepath.Join(dir, name), fst.Mode().Perm())
			return
		} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
			rep.warn("dataDir", "%v", err)
			return
		}
	}
	rep.ok("dataDir", "(writable, mode %04o)", mode)
}

// localAPIServer derives a URL for the local API listener.
func localAPIServer(cfg *config.Config) string {
	addr := cfg.Listeners.API.Address
	if addr == "" {
		addr = "0.0.0.0:8080"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://127.0.0.1:8080"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// checkAPI probes the API server, the token and the diagnoses. It reports
// whether a server answered.
func (o *cliOpts) checkAPI(ctx context.Context, rep *doctorReport, cfg *config.Config) bool {
	r, err := o.resolve()
	explicit := err == nil
	if err != nil {
		r = cli.Resolved{Server: localAPIServer(cfg)}
	}
	saved := o.timeout
	o.timeout = 5 * time.Second
	defer func() { o.timeout = saved }()
	c, err := o.newClient(r)
	if err != nil {
		rep.fail("api", "%v", err)
		return false
	}
	start := time.Now()
	var ver struct {
		Version string `json:"version"`
	}
	err = c.Get(ctx, "/version", nil, &ver)
	var plainToTLS *apiclient.Error
	if !explicit && errors.As(err, &plainToTLS) && plainToTLS.StatusCode == 400 && strings.Contains(plainToTLS.Message, "HTTPS") {
		// The local listener terminates TLS (listeners.api.tls): probe it
		// over https; a self-signed loopback certificate is not verified.
		r.Server, r.InsecureSkipVerify = "https://"+strings.TrimPrefix(r.Server, "http://"), true
		if c, err = o.newClient(r); err != nil {
			rep.fail("api", "%v", err)
			return false
		}
		err = c.Get(ctx, "/version", nil, &ver)
	}
	if err != nil {
		var apiErr *apiclient.Error
		switch {
		case errors.As(err, &apiErr) && apiErr.StatusCode == 401:
			// /version behind auth: reachable all the same.
		case explicit:
			rep.fail("api", "%s unreachable: %v", r.Server, err)
			return false
		default:
			rep.warn("api", "no CLI context and nothing answers at %s (%v); run `kudurru login` to check a remote server", r.Server, err)
			return false
		}
	}
	rep.ok("api", "%s (kudurru %s, %d ms)", r.Server, firstNonEmpty(ver.Version, "?"), time.Since(start).Milliseconds())

	if r.Token == "" {
		rep.warn("token", "no token (context %q); authenticated checks skipped", r.ContextName)
		return true
	}
	me, err := c.Me(ctx)
	if err != nil {
		rep.fail("token", "rejected (%s token): %v", r.TokenSource, err)
		return true
	}
	rep.ok("token", "%s %s, roles %s", me.Principal.Kind, firstNonEmpty(me.Principal.Name, me.Principal.ID), firstNonEmpty(strings.Join(me.Roles, ","), "none"))

	ds, err := diagnoses(ctx, c)
	if err != nil {
		if apiclient.IsStatus(err, 403) {
			rep.warn("diagnoses", "not permitted for this token: %v", err)
		} else {
			rep.warn("diagnoses", "%v", err)
		}
		return true
	}
	errs := 0
	for _, d := range ds {
		if severityRank(d.Severity) == 0 {
			errs++
		}
	}
	switch {
	case len(ds) == 0:
		rep.ok("diagnoses", "none")
	case errs > 0:
		rep.fail("diagnoses", "%d messages, %d errors", len(ds), errs)
	default:
		rep.warn("diagnoses", "%d messages", len(ds))
	}
	for _, d := range ds {
		k, ns, n := d.ref()
		obj := ""
		if n != "" {
			obj = " [" + cli.ObjectRef(firstNonEmpty(k, "object"), ns, n) + "]"
		}
		hint := ""
		if d.Hint != "" {
			hint = " (hint: " + d.Hint + ")"
		}
		rep.say("  %s %s: %s%s%s\n", strings.ToUpper(firstNonEmpty(d.Severity, "info")), firstNonEmpty(d.Reason, "-"), d.Message, obj, hint)
	}
	return true
}

// listenerPorts returns every address and protocol the config binds.
func listenerPorts(cfg *config.Config) [][2]string {
	var out [][2]string
	for _, l := range cfg.Listeners.DNS {
		for _, p := range l.Protocols {
			out = append(out, [2]string{l.Address, p})
		}
	}
	for _, l := range cfg.Listeners.DoT {
		out = append(out, [2]string{l.Address, "tcp"})
	}
	for _, l := range cfg.Listeners.DoH {
		out = append(out, [2]string{l.Address, "tcp"})
	}
	for _, l := range cfg.Listeners.DoQ {
		out = append(out, [2]string{l.Address, "udp"})
	}
	for _, l := range cfg.Listeners.DoH3 {
		out = append(out, [2]string{l.Address, "udp"})
	}
	for _, a := range []string{cfg.Listeners.API.Address, cfg.Listeners.Ops.Address, cfg.Listeners.Metrics.Address, cfg.Listeners.Internal.Address} {
		if a != "" {
			out = append(out, [2]string{a, "tcp"})
		}
	}
	return out
}

// checkPorts tries to bind every listener. In use is fine when a kudurru
// server answered; permission denied means the binary lacks
// CAP_NET_BIND_SERVICE for ports below 1024.
func checkPorts(rep *doctorReport, cfg *config.Config, serverUp bool) {
	var inUse []string
	for _, p := range listenerPorts(cfg) {
		addr, proto := p[0], p[1]
		check := "port " + addr + "/" + proto
		err := tryBind(proto, addr)
		switch {
		case err == nil:
			rep.ok(check, "bindable")
		case errors.Is(err, syscall.EADDRINUSE):
			if serverUp {
				inUse = append(inUse, addr+"/"+proto)
				continue
			}
			rep.warn(check, "in use by another process (is kudurru already running?)")
		case errors.Is(err, syscall.EACCES), errors.Is(err, syscall.EPERM):
			rep.warn(check, "permission denied for uid %d: ports below 1024 need root or CAP_NET_BIND_SERVICE (the image grants it)", os.Getuid())
		case errors.Is(err, syscall.EADDRNOTAVAIL):
			rep.fail(check, "address not available on this host")
		default:
			rep.warn(check, "%v", err)
		}
	}
	if len(inUse) > 0 {
		rep.ok("ports", "%d in use by the running server (%s)", len(inUse), strings.Join(inUse, ", "))
	}
}

func tryBind(proto, addr string) error {
	switch proto {
	case "udp":
		pc, err := net.ListenPacket("udp", addr)
		if err != nil {
			return err
		}
		return pc.Close()
	default:
		l, err := net.Listen("tcp", addr)
		if err != nil {
			return err
		}
		return l.Close()
	}
}

// checkDatabase checks the central database (database.dsn, D158): reachable,
// PostgreSQL 15 or newer, a schema this binary can use. It reads only;
// the server migrates.
func checkDatabase(ctx context.Context, rep *doctorReport, cfg *config.Config) {
	if !cfg.Database.Enabled() {
		return
	}
	pc, err := central.ParseDSN(cfg.Database.DSN)
	if err != nil {
		rep.fail("database", "%v", err)
		return
	}
	cc := pc.ConnConfig
	if cc.TLSConfig == nil && cc.Host != "" && !strings.HasPrefix(cc.Host, "/") && cc.Host != "localhost" && cc.Host != "127.0.0.1" && cc.Host != "::1" {
		rep.warn("database", "sslmode=disable to %s: the connection is not encrypted; use sslmode=verify-full", cc.Host)
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, cc)
	if err != nil {
		rep.fail("database", "cannot connect to %s:%d: %v (nodes keep their data locally until it answers)", cc.Host, cc.Port, err)
		return
	}
	defer func() { _ = conn.Close(context.Background()) }()
	var num int
	if err := conn.QueryRow(ctx, "SELECT current_setting('server_version_num')::int").Scan(&num); err != nil {
		rep.fail("database", "server version: %v", err)
		return
	}
	if num < central.MinServerVersion {
		rep.fail("database", "PostgreSQL %d.%d is too old; kudurru needs 15 or newer", num/10000, num%10000)
		return
	}
	var schema, compat string
	err = conn.QueryRow(ctx, `SELECT coalesce(max(value) FILTER (WHERE key = 'schema_version'), ''), coalesce(max(value) FILTER (WHERE key = 'compat_version'), '') FROM kudurru_meta`).Scan(&schema, &compat)
	switch {
	case err != nil:
		rep.ok("database", "PostgreSQL %d.%d at %s; no kudurru schema yet (the server creates it)", num/10000, num%10000, cc.Host)
	case atoi(compat) > central.SchemaVersion:
		rep.fail("database", "schema %s needs a newer kudurru (compat %s, this binary %d)", schema, compat, central.SchemaVersion)
	default:
		rep.ok("database", "PostgreSQL %d.%d at %s, schema %s", num/10000, num%10000, cc.Host, schema)
	}
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
