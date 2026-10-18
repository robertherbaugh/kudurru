package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"sort"
	"strings"

	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/v2"

	"github.com/robertherbaugh/kudurru/internal/audit"
	"github.com/robertherbaugh/kudurru/internal/config"
)

// Redacted replaces writeOnly values in settings responses.
const Redacted = "<redacted>"

// SettingKey is one row of GET /settings/_schema (the config metadata
// table, internal/config.Schema).
type SettingKey struct {
	Path            string `json:"path"`
	Type            string `json:"type"`
	Scope           string `json:"scope" enum:"node,cluster"`
	Default         any    `json:"default"`
	Help            string `json:"help"`
	HostOnly        bool   `json:"hostOnly"`
	EnvLocked       bool   `json:"envLocked"`
	RestartRequired bool   `json:"restartRequired"`
	WriteOnly       bool   `json:"writeOnly"`
	// Label is the human name ("Default block response"); Group the UI
	// section ("Blocking").
	Label string `json:"label,omitempty"`
	Group string `json:"group,omitempty"`
	SettingConstraints
	// Value is the effective value (Redacted for writeOnly keys).
	Value any `json:"value"`
}

// SettingConstraints is the typed-control metadata of a key or a nested
// value: PATCH /settings rejects values outside it (400 invalid).
type SettingConstraints struct {
	// Enum is the closed set of a string value.
	Enum []SettingOption `json:"enum,omitempty"`
	// Min and Max bound an int value (inclusive).
	Min *int `json:"min,omitempty"`
	Max *int `json:"max,omitempty"`
	// Presets are suggested values (durations, byte sizes).
	Presets []string `json:"presets,omitempty"`
	// Format is the syntax of a string value; empty values are not checked.
	Format string `json:"format,omitempty" enum:"ip,ipv4,ipv6,cidr,hostport,url,domain,upstream,email"`
	// Ref is the kind of object the value names.
	Ref string `json:"ref,omitempty" enum:"upstreamGroup,clientGroup,blockList,policy,zone"`
	// Items is the element shape of a list or string[]; Keys and Values
	// the key and value shapes of a map.
	Items  *SettingShape `json:"items,omitempty"`
	Keys   *SettingShape `json:"keys,omitempty"`
	Values *SettingShape `json:"values,omitempty"`
	// Fields are the members of an object.
	Fields []SettingField `json:"fields,omitempty"`
}

// SettingOption is one value of a closed set.
type SettingOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
	Help  string `json:"help,omitempty"`
}

// SettingShape describes a value nested in a key (a list element, a map
// key or value, an object member).
type SettingShape struct {
	Type string `json:"type" enum:"string,int,bool,duration,bytes,string[],list,map,object"`
	// Default is what an omitted or empty value means.
	Default any `json:"default,omitempty"`
	SettingConstraints
}

// SettingField is one member of an object shape.
type SettingField struct {
	Name  string `json:"name"`
	Label string `json:"label,omitempty"`
	Help  string `json:"help,omitempty"`
	// Required members must be present and non-empty.
	Required bool `json:"required,omitempty"`
	SettingShape
}

func settingConstraints(s config.Shape) SettingConstraints {
	out := SettingConstraints{Min: s.Min, Max: s.Max, Presets: s.Presets, Format: string(s.Format), Ref: string(s.Ref),
		Items: settingShape(s.Items), Keys: settingShape(s.Keys), Values: settingShape(s.Values)}
	for _, o := range s.Enum {
		out.Enum = append(out.Enum, SettingOption{Value: o.Value, Label: o.Label, Help: o.Help})
	}
	for _, f := range s.Fields {
		out.Fields = append(out.Fields, SettingField{Name: f.Name, Label: f.Label, Help: f.Help, Required: f.Required, SettingShape: *settingShape(&f.Shape)})
	}
	return out
}

func settingShape(s *config.Shape) *SettingShape {
	if s == nil {
		return nil
	}
	return &SettingShape{Type: string(s.Type), Default: s.Default, SettingConstraints: settingConstraints(*s)}
}

type schemaOutput struct{ Body *List[SettingKey] }

// SettingsBody is GET|PATCH /settings and /node/config: the keys of one
// scope as a nested document (writeOnly values redacted).
type SettingsBody struct {
	Scope    string         `json:"scope" enum:"node,cluster"`
	Node     string         `json:"node"`
	Settings map[string]any `json:"settings"`
	// Changed lists the dotted keys a PATCH changed; RestartRequired the
	// ones that only apply after a restart.
	Changed         []string `json:"changed,omitempty"`
	RestartRequired []string `json:"restartRequired,omitempty"`
	// Persisted is false when the change lives in memory only (node
	// scope, or no settings file is configured; it is lost on restart).
	Persisted *bool `json:"persisted,omitempty"`
	Took
}

type settingsOutput struct{ Body *SettingsBody }

type patchSettingsInput struct {
	Body map[string]any
}

func (s *Server) registerSettings() {
	register(s, operation(http.MethodGet, "/settings", "getSettings", "Cluster-scope settings (redacted)", "settings"),
		perm{resource: "settings", verb: "read"}, func(context.Context, *struct{}) (*settingsOutput, error) {
			return s.settingsBody(config.ScopeCluster, nil, nil), nil
		})
	register(s, operation(http.MethodPatch, "/settings", "patchSettings", "Change cluster-scope settings (JSON merge patch)", "settings"),
		perm{resource: "settings", verb: "write"}, func(ctx context.Context, in *patchSettingsInput) (*settingsOutput, error) {
			return s.patchSettings(ctx, config.ScopeCluster, in.Body)
		})
	register(s, operation(http.MethodGet, "/node/config", "getNodeConfig", "This node's host-local settings (redacted)", "settings"),
		perm{resource: "settings", verb: "read"}, func(context.Context, *struct{}) (*settingsOutput, error) {
			return s.settingsBody(config.ScopeNode, nil, nil), nil
		})
	register(s, operation(http.MethodPatch, "/node/config", "patchNodeConfig", "Change this node's settings (hostOnly keys refused)", "settings"),
		perm{resource: "node", verb: "config"}, func(ctx context.Context, in *patchSettingsInput) (*settingsOutput, error) {
			return s.patchSettings(ctx, config.ScopeNode, in.Body)
		})
	register(s, operation(http.MethodGet, "/settings/_schema", "settingsSchema", "The settings metadata table", "settings"),
		perm{resource: "settings", verb: "read"}, s.settingsSchema)
}

// flatSettings renders cfg as dotted schema path → value, using the JSON
// form of the config structs (whose tags equal the koanf keys).
func flatSettings(cfg *config.Config) map[string]any {
	raw, err := json.Marshal(cfg)
	if err != nil {
		return map[string]any{}
	}
	var tree map[string]any
	if err := json.Unmarshal(raw, &tree); err != nil {
		return map[string]any{}
	}
	out := make(map[string]any, len(config.Schema))
	for _, k := range config.Schema {
		if v, ok := lookupPath(tree, k.Path); ok {
			out[k.Path] = v
		} else {
			out[k.Path] = k.Default
		}
	}
	return out
}

func lookupPath(tree map[string]any, p string) (any, bool) {
	var cur any = tree
	for _, seg := range strings.Split(p, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[seg]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

func setPath(tree map[string]any, p string, v any) {
	segs := strings.Split(p, ".")
	cur := tree
	for _, seg := range segs[:len(segs)-1] {
		next, ok := cur[seg].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[seg] = next
		}
		cur = next
	}
	cur[segs[len(segs)-1]] = v
}

// settingsBody renders the keys of one scope.
func (s *Server) settingsBody(scope config.Scope, changed, restart []string) *settingsOutput {
	cfg := s.config()
	flat := flatSettings(cfg)
	tree := map[string]any{}
	for _, k := range config.Schema {
		if k.Scope != scope {
			continue
		}
		v := flat[k.Path]
		if k.WriteOnly {
			v = Redacted
		}
		setPath(tree, k.Path, v)
	}
	return &settingsOutput{Body: &SettingsBody{Scope: string(scope), Node: s.opts.NodeName, Settings: tree, Changed: changed, RestartRequired: restart}}
}

func (s *Server) settingsSchema(context.Context, *struct{}) (*schemaOutput, error) {
	cfg := s.config()
	flat := flatSettings(cfg)
	items := make([]SettingKey, 0, len(config.Schema))
	for _, k := range config.Schema {
		v := flat[k.Path]
		if k.WriteOnly {
			v = Redacted
		}
		items = append(items, SettingKey{
			Path: k.Path, Type: string(k.Type), Scope: string(k.Scope), Default: k.Default, Help: k.Help,
			HostOnly: k.HostOnly, EnvLocked: slices.Contains(cfg.EnvLocked, k.Path), RestartRequired: k.RestartRequired,
			WriteOnly: k.WriteOnly, Label: k.Label, Group: k.Group, SettingConstraints: settingConstraints(k.Shape()), Value: v,
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Path < items[j].Path })
	return &schemaOutput{Body: newList(items, "")}, nil
}

// patchPaths flattens a merge patch into schema paths. A path is a leaf
// when the schema names it; objects above a leaf recurse.
func patchPaths(prefix string, patch map[string]any, out map[string]any) error {
	for k, v := range patch {
		p := k
		if prefix != "" {
			p = prefix + "." + k
		}
		if _, ok := config.Lookup(p); ok {
			out[p] = v
			continue
		}
		sub, ok := v.(map[string]any)
		if !ok || !hasSchemaPrefix(p+".") {
			return errInvalid("%s: unknown settings key", p)
		}
		if err := patchPaths(p, sub, out); err != nil {
			return err
		}
	}
	return nil
}

func hasSchemaPrefix(prefix string) bool {
	for _, k := range config.Schema {
		if strings.HasPrefix(k.Path, prefix) {
			return true
		}
	}
	return false
}

// patchSettings applies a JSON merge patch over the keys of one scope:
// unknown keys and keys of the other scope are 400 invalid, hostOnly keys
// 400 host_only, env-locked keys 409 env_locked; the result is decoded and
// validated like the file (400 invalid with the key), then persisted by
// the SettingsWriter and served from memory.
func (s *Server) patchSettings(ctx context.Context, scope config.Scope, patch map[string]any) (*settingsOutput, error) {
	if len(patch) == 0 {
		return nil, errInvalid("body: an object with the keys to change is required")
	}
	// nosemgrep: iterate-over-empty-map -- patchPaths fills changes
	changes := map[string]any{}
	if err := patchPaths("", patch, changes); err != nil {
		return nil, err
	}
	cur := s.config()
	var paths []string
	for p := range changes {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var restart []string
	for _, p := range paths {
		k, _ := config.Lookup(p)
		switch {
		case k.HostOnly:
			return nil, newError(http.StatusBadRequest, KeyHostOnly, p+" is hostOnly and can only be set in the config file or environment on the host",
				"edit kudurru.yaml or KUDURRU_* on the node")
		case k.Scope != scope && scope == config.ScopeCluster:
			return nil, errInvalid("%s: a node-scope key; use PATCH /api/v1/node/config", p)
		case k.Scope != scope:
			return nil, errInvalid("%s: a cluster-scope key; use PATCH /api/v1/settings", p)
		case slices.Contains(cur.EnvLocked, p):
			return nil, newError(http.StatusConflict, KeyEnvLocked, p+" is set by the environment and read-only here", "unset KUDURRU_* for it on the node")
		}
		if k.RestartRequired {
			restart = append(restart, p)
		}
	}
	flat := flatSettings(cur)
	for p, v := range changes {
		k, _ := config.Lookup(p)
		switch {
		case v == nil:
			v = k.Default
		case k.Type == config.TypeMap:
			// JSON merge patch (RFC 7386) inside map-valued keys.
			if pm, ok := v.(map[string]any); ok {
				v = mergePatch(flat[p], pm)
			}
		}
		flat[p] = v
	}
	next, err := decodeSettings(flat)
	if err != nil {
		return nil, err
	}
	next.EnvLocked, next.Source = cur.EnvLocked, cur.Source
	before := map[string]any{}
	after := map[string]any{}
	old := flatSettings(cur)
	for _, p := range paths {
		k, _ := config.Lookup(p)
		before[p], after[p] = old[p], flatSettings(next)[p]
		if k.WriteOnly {
			before[p], after[p] = Redacted, Redacted
		}
	}
	persisted := false
	if s.opts.Settings != nil && scope == config.ScopeCluster {
		if err := s.opts.Settings.WriteSettings(ctx, next, paths); err != nil {
			s.recordAction(ctx, "settings.write", audit.Resource{Kind: "settings", Name: string(scope)}, err, before, after)
			return nil, storeError(err)
		}
		persisted = true
	}
	if s.opts.Reload != nil {
		next.SettingsFile = cur.SettingsFile
		next.Node.Name = cur.Node.Name
		s.opts.Reload.Set(next)
	} else {
		s.cfgMu.Lock()
		s.cfg = next
		s.cfgMu.Unlock()
	}
	action := "settings.write"
	if scope == config.ScopeNode {
		action = "node.config"
	}
	s.recordAction(ctx, action, audit.Resource{Kind: "settings", Name: string(scope)}, nil, before, after)
	out := s.settingsBody(scope, paths, restart)
	out.Body.Persisted = &persisted
	return out, nil
}

// decodeSettings decodes a flat key map into a validated Config.
func decodeSettings(flat map[string]any) (*config.Config, error) {
	k := koanf.New(".")
	if err := k.Load(confmap.Provider(flat, "."), nil); err != nil {
		return nil, errInvalid("settings: %v", err)
	}
	var cfg config.Config
	if err := k.Unmarshal("", &cfg); err != nil {
		return nil, errInvalid("settings: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, settingsInvalid(err)
	}
	return &cfg, nil
}

// settingsInvalid is the 400 invalid of a settings value that fails
// validation: the message names every key, Fields lists the paths that
// violate their metadata and Hint the allowed values when one closed set
// was missed.
func settingsInvalid(err error) *Error {
	e := errInvalid("settings: %v", err)
	fes := config.FieldErrors(err)
	for _, fe := range fes {
		if !slices.Contains(e.Body.Fields, fe.Path) {
			e.Body.Fields = append(e.Body.Fields, fe.Path)
		}
	}
	if len(fes) == 1 && len(fes[0].Allowed) > 0 {
		e.Body.Hint = fes[0].Path + " must be one of: " + strings.Join(fes[0].Allowed, ", ")
	}
	return e
}

// ErrSettingsManaged is the error a SettingsWriter returns when the
// cluster settings are not owned by this process (409 managed_by): in
// Kubernetes they are the kudurru-settings ConfigMap, so a PATCH would be
// overwritten by the next Helm upgrade and never reach the other pods.
func ErrSettingsManaged(owner, hint string) error {
	return newError(http.StatusConflict, KeyManagedBy, "cluster settings are managed by "+owner+"; change them there", hint)
}

// SettingsFunc adapts a function to SettingsWriter.
type SettingsFunc func(ctx context.Context, next *config.Config, changed []string) error

// WriteSettings implements SettingsWriter.
func (f SettingsFunc) WriteSettings(ctx context.Context, next *config.Config, changed []string) error {
	return f(ctx, next, changed)
}

// mergePatch applies an RFC 7386 merge patch to cur.
func mergePatch(cur any, patch map[string]any) any {
	base, ok := cur.(map[string]any)
	out := map[string]any{}
	if ok {
		for k, v := range base {
			out[k] = v
		}
	}
	for k, v := range patch {
		switch pv := v.(type) {
		case nil:
			delete(out, k)
		case map[string]any:
			out[k] = mergePatch(out[k], pv)
		default:
			out[k] = v
		}
	}
	return out
}

// toJSON renders v for comparisons and logs.
func toJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}
