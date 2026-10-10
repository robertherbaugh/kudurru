package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
)

// EnvPrefix is the prefix of every kudurru environment variable. Variables
// with "__" map onto settings keys (KUDURRU_NODE__NAME → node.name); the
// rest (KUDURRU_ADMIN_PASSWORD_FILE, KUDURRU_CONFIG) are reserved.
const EnvPrefix = "KUDURRU_"

// Config is the typed view of kudurru.yaml. Node-scoped sections are host
// local; cluster-scoped sections are the Kudurru singleton.
type Config struct {
	Node      Node      `koanf:"node" json:"node"`
	Listeners Listeners `koanf:"listeners" json:"listeners"`
	Upstreams Upstreams `koanf:"upstreams" json:"upstreams"`
	Log       Log       `koanf:"log" json:"log"`

	// EnvLocked lists the key paths that came from the environment.
	EnvLocked []string `koanf:"-" json:"-"`
	// Source is the file the config was read from, or "" for defaults only.
	Source string `koanf:"-" json:"-"`
}

// Node identifies this instance.
type Node struct {
	Name    string `koanf:"name" json:"name"`
	DataDir string `koanf:"dataDir" json:"dataDir"`
}

// Listeners are the bound addresses.
type Listeners struct {
	DNS               []DNSListener `koanf:"dns" json:"dns"`
	API               Listener      `koanf:"api" json:"api"`
	Ops               Listener      `koanf:"ops" json:"ops"`
	Metrics           Listener      `koanf:"metrics" json:"metrics"`
	ProxyProtocolFrom []string      `koanf:"proxyProtocolFrom" json:"proxyProtocolFrom"`
}

// DNSListener is one DNS bind.
type DNSListener struct {
	Address   string   `koanf:"address" json:"address"`
	Protocols []string `koanf:"protocols" json:"protocols"`
}

// Listener is an HTTP bind.
type Listener struct {
	Address string `koanf:"address" json:"address"`
}

// Upstreams configures forwarding.
type Upstreams struct {
	Groups      map[string]UpstreamGroup `koanf:"groups" json:"groups"`
	Bootstrap   []string                 `koanf:"bootstrap" json:"bootstrap"`
	Conditional map[string]string        `koanf:"conditional" json:"conditional"`
}

// UpstreamGroup is a named set of servers with a strategy.
type UpstreamGroup struct {
	Servers  []string `koanf:"servers" json:"servers"`
	Strategy string   `koanf:"strategy" json:"strategy"`
	Timeout  Duration `koanf:"timeout" json:"timeout"`
}

// Duration is a time.Duration that reads and writes as "2s" in YAML, JSON
// and the environment.
type Duration time.Duration

// UnmarshalText implements encoding.TextUnmarshaler.
func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// MarshalText implements encoding.TextMarshaler.
func (d Duration) MarshalText() ([]byte, error) { return []byte(time.Duration(d).String()), nil }

// String implements fmt.Stringer.
func (d Duration) String() string { return time.Duration(d).String() }

// Std returns the time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// Log configures logging.
type Log struct {
	Level  string `koanf:"level" json:"level"`
	Format string `koanf:"format" json:"format"`
}

// Load reads defaults, then the file (if path is non-empty), then the
// environment overrides, and validates the result.
func Load(path string, environ []string) (*Config, error) {
	k := koanf.New(".")
	if err := k.Load(confmap.Provider(Defaults(), "."), nil); err != nil {
		return nil, fmt.Errorf("config: defaults: %w", err)
	}
	if path != "" {
		if err := k.Load(file.Provider(path), yaml.Parser()); err != nil {
			return nil, fmt.Errorf("config: read %s: %w", path, err)
		}
	}
	overrides, locked, err := envOverrides(environ)
	if err != nil {
		return nil, err
	}
	if err := k.Load(confmap.Provider(overrides, "."), nil); err != nil {
		return nil, fmt.Errorf("config: environment: %w", err)
	}

	var cfg Config
	if err := k.Unmarshal("", &cfg); err != nil {
		return nil, fmt.Errorf("config: decode: %w", err)
	}
	cfg.EnvLocked = locked
	cfg.Source = path
	if cfg.Node.Name == "" {
		if h, err := os.Hostname(); err == nil {
			cfg.Node.Name = h
		} else {
			cfg.Node.Name = "kudurru"
		}
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Validate checks invariants the schema cannot express.
func (c *Config) Validate() error {
	var errs []error
	if len(c.Listeners.DNS) == 0 {
		errs = append(errs, errors.New("listeners.dns: at least one listener is required"))
	}
	for i, l := range c.Listeners.DNS {
		if l.Address == "" {
			errs = append(errs, fmt.Errorf("listeners.dns[%d].address is required", i))
		}
		for _, p := range l.Protocols {
			if p != "udp" && p != "tcp" {
				errs = append(errs, fmt.Errorf("listeners.dns[%d]: unsupported protocol %q (udp, tcp)", i, p))
			}
		}
	}
	if _, ok := c.Upstreams.Groups["default"]; !ok {
		errs = append(errs, errors.New("upstreams.groups.default is required"))
	}
	for name, g := range c.Upstreams.Groups {
		if len(g.Servers) == 0 {
			errs = append(errs, fmt.Errorf("upstreams.groups.%s: servers is empty", name))
		}
		switch g.Strategy {
		case "", "parallel_best", "strict", "random", "rtt_weighted":
		default:
			errs = append(errs, fmt.Errorf("upstreams.groups.%s: unknown strategy %q", name, g.Strategy))
		}
	}
	for suffix, group := range c.Upstreams.Conditional {
		if _, ok := c.Upstreams.Groups[group]; !ok {
			errs = append(errs, fmt.Errorf("upstreams.conditional[%s]: unknown group %q", suffix, group))
		}
	}
	switch strings.SplitN(c.Log.Level, ",", 2)[0] {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("log.level: unknown level %q", c.Log.Level))
	}
	return errors.Join(errs...)
}

// envOverrides maps KUDURRU_<SECTION>__<KEY>[_FILE]=value onto schema paths.
func envOverrides(environ []string) (map[string]any, []string, error) {
	paths := lowerPaths()
	out := map[string]any{}
	var locked []string
	for _, kv := range environ {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || !strings.HasPrefix(name, EnvPrefix) || !strings.Contains(name, "__") {
			continue
		}
		rest := strings.TrimPrefix(name, EnvPrefix)
		fromFile := false
		if strings.HasSuffix(rest, "_FILE") {
			rest = strings.TrimSuffix(rest, "_FILE")
			fromFile = true
		}
		lower := strings.ToLower(strings.ReplaceAll(rest, "__", "."))
		path, known := paths[lower]
		if !known {
			return nil, nil, fmt.Errorf("config: environment variable %s does not match a settings key", name)
		}
		if fromFile {
			b, err := os.ReadFile(value) //nolint:gosec // the operator names the file on purpose (KUDURRU_*_FILE)
			if err != nil {
				return nil, nil, fmt.Errorf("config: %s: %w", name, err)
			}
			value = strings.TrimSpace(string(b))
		}
		key, _ := Lookup(path)
		typed, err := coerce(key.Type, value)
		if err != nil {
			return nil, nil, fmt.Errorf("config: %s: %w", name, err)
		}
		out[path] = typed
		locked = append(locked, path)
	}
	return out, locked, nil
}

func coerce(t Type, v string) (any, error) {
	switch t {
	case TypeInt:
		return strconv.Atoi(v)
	case TypeBool:
		return strconv.ParseBool(v)
	case TypeStringList:
		if v == "" {
			return []string{}, nil
		}
		parts := strings.Split(v, ",")
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		return parts, nil
	case TypeList, TypeMap:
		return nil, fmt.Errorf("list and map keys cannot be set from the environment; use the config file")
	default:
		return v, nil
	}
}
