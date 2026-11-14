package config

import (
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// The HTTP batch sinks of querylog.sinks (D371): ClickHouse and Loki. Their
// settings say where the queries go and which credential to present. A
// credential is always a reference (CredentialRef); its value never enters
// the settings, the API or the cluster document. Everything a sink factory
// would refuse is refused here, so that an admitted settings document can
// always be built: the factories call the same Validate methods.

// Types of querylog.sinks beside QuerylogSinkSlog.
const (
	// QuerylogSinkClickHouse inserts batches into a ClickHouse table over
	// the HTTP interface (INSERT ... FORMAT JSONEachRow).
	QuerylogSinkClickHouse = "clickhouse"
	// QuerylogSinkLoki pushes batches to Grafana Loki (/loki/api/v1/push).
	QuerylogSinkLoki = "loki"
)

// Defaults of the HTTP sinks.
const (
	DefaultQuerylogClickHouseDatabase = "kudurru"
	DefaultQuerylogClickHouseTable    = "queries"
	// DefaultQuerylogLokiJob is the job label of a Loki sink without labels.
	DefaultQuerylogLokiJob = "kudurru"
	// QuerylogCompressionGzip (the default) sends gzip request bodies;
	// QuerylogCompressionNone sends them as they are.
	QuerylogCompressionGzip = "gzip"
	QuerylogCompressionNone = "none"
	// MaxQuerylogLokiLabels bounds the fixed labels of a Loki sink.
	MaxQuerylogLokiLabels = 16
)

// Labels a Loki sink sets itself on every stream; querylog.sinks[].loki.labels
// may not name them.
const (
	QuerylogLokiLabelNode        = "node"
	QuerylogLokiLabelStatusClass = "status_class"
)

// QuerylogClickHouse is the clickhouse object of a querylog.sinks entry.
type QuerylogClickHouse struct {
	// URL is the base URL of the ClickHouse HTTP interface, e.g.
	// https://clickhouse.example:8443.
	URL string `koanf:"url" json:"url"`
	// Database and Table name the destination table; empty means
	// kudurru.queries.
	Database string `koanf:"database" json:"database,omitempty"`
	Table    string `koanf:"table" json:"table,omitempty"`
	// Username and PasswordRef are sent as HTTP basic authentication.
	Username    string         `koanf:"username" json:"username,omitempty"`
	PasswordRef *CredentialRef `koanf:"passwordRef" json:"passwordRef,omitempty"`
	// CARef names a PEM bundle that replaces the system trust store for
	// this destination.
	CARef *CredentialRef `koanf:"caRef" json:"caRef,omitempty"`
	// Compression is gzip (default) or none.
	Compression string `koanf:"compression" json:"compression,omitempty"`
}

// QuerylogLoki is the loki object of a querylog.sinks entry.
type QuerylogLoki struct {
	// URL is the base URL of Loki or its gateway, e.g.
	// https://loki.example:3100; /loki/api/v1/push is appended.
	URL string `koanf:"url" json:"url"`
	// Tenant is sent as X-Scope-OrgID.
	Tenant string `koanf:"tenant" json:"tenant,omitempty"`
	// Username and PasswordRef are sent as HTTP basic authentication;
	// BearerTokenRef as a bearer token. At most one of the two forms.
	Username       string         `koanf:"username" json:"username,omitempty"`
	PasswordRef    *CredentialRef `koanf:"passwordRef" json:"passwordRef,omitempty"`
	BearerTokenRef *CredentialRef `koanf:"bearerTokenRef" json:"bearerTokenRef,omitempty"`
	// CARef names a PEM bundle that replaces the system trust store for
	// this destination.
	CARef *CredentialRef `koanf:"caRef" json:"caRef,omitempty"`
	// Labels are fixed stream labels; empty means job=kudurru.
	Labels map[string]string `koanf:"labels" json:"labels,omitempty"`
	// StructuredMetadata attaches client, domain and the other per-query
	// fields as structured metadata; nil means true.
	StructuredMetadata *bool `koanf:"structuredMetadata" json:"structuredMetadata,omitempty"`
	// Compression is gzip (default) or none.
	Compression string `koanf:"compression" json:"compression,omitempty"`
}

// DatabaseName returns the database with the default applied.
func (c QuerylogClickHouse) DatabaseName() string {
	if c.Database == "" {
		return DefaultQuerylogClickHouseDatabase
	}
	return c.Database
}

// TableName returns the table with the default applied.
func (c QuerylogClickHouse) TableName() string {
	if c.Table == "" {
		return DefaultQuerylogClickHouseTable
	}
	return c.Table
}

// StreamLabels returns the fixed labels with the default applied.
func (l QuerylogLoki) StreamLabels() map[string]string {
	if len(l.Labels) == 0 {
		return map[string]string{"job": DefaultQuerylogLokiJob}
	}
	out := make(map[string]string, len(l.Labels))
	for k, v := range l.Labels {
		out[k] = v
	}
	return out
}

// UsesStructuredMetadata reports whether entries carry structured metadata.
func (l QuerylogLoki) UsesStructuredMetadata() bool {
	return l.StructuredMetadata == nil || *l.StructuredMetadata
}

// GzipBody reports whether a compression setting means gzip request bodies.
func GzipBody(compression string) bool { return compression != QuerylogCompressionNone }

// ValidCompression reports whether compression is a value of the setting's
// closed set. The settings shape enforces the set at admission; a sink
// factory checks it again.
func ValidCompression(compression string) bool {
	return compression == "" || compression == QuerylogCompressionGzip || compression == QuerylogCompressionNone
}

var (
	// clickHouseIdentifier keeps database and table names to what needs no
	// quoting rules: the names are written into the INSERT statement.
	clickHouseIdentifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,62}$`)
	lokiLabelName        = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)
	// lokiTenant is the tenant ID alphabet of Loki, without the | of
	// multi-tenant reads.
	lokiTenant       = regexp.MustCompile(`^[A-Za-z0-9!_.*'()-]{1,150}$`)
	secretObjectName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$`)
	secretObjectKey  = regexp.MustCompile(`^[-._A-Za-z0-9]{1,253}$`)
)

// ParseSinkURL parses the base URL of an HTTP sink: http or https with a
// host, and nothing that could carry a credential or change the request the
// sink builds (user information, query, fragment, dot segments). Its errors
// complete the sentence "<path> ..." and never repeat the URL.
func ParseSinkURL(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, fmt.Errorf("is required")
	}
	if len(raw) > 2048 || strings.ContainsFunc(raw, func(r rune) bool { return r <= ' ' || r == 0x7f || r == '\\' }) {
		return nil, fmt.Errorf("is not an http or https URL of at most 2048 characters without spaces")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return nil, fmt.Errorf("is not an http or https URL")
	}
	if u.User != nil {
		return nil, fmt.Errorf("must not carry a username or password; use username and passwordRef")
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" {
		return nil, fmt.Errorf("must not carry a query or fragment")
	}
	if port := u.Port(); port != "" {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("has an invalid port")
		}
	}
	if u.RawPath != "" {
		return nil, fmt.Errorf("must not use percent-encoding in its path")
	}
	for _, segment := range strings.Split(u.Path, "/") {
		if segment == "." || segment == ".." {
			return nil, fmt.Errorf("must not contain . or .. path segments")
		}
	}
	return u, nil
}

// validate checks a credential reference: exactly one form, each in the
// syntax the node's resolver accepts.
func (r *CredentialRef) validate(at string) []error {
	forms := 0
	for _, set := range []bool{r.SecretRef != nil, r.File != "", r.Env != ""} {
		if set {
			forms++
		}
	}
	if forms != 1 {
		return []error{fmt.Errorf("%s: set exactly one of secretRef, file or env", at)}
	}
	switch {
	case r.SecretRef != nil:
		var errs []error
		if !secretObjectName.MatchString(r.SecretRef.Name) {
			errs = append(errs, fmt.Errorf("%s.secretRef.name: %q is not the name of a Secret", at, r.SecretRef.Name))
		}
		if !secretObjectKey.MatchString(r.SecretRef.Key) {
			errs = append(errs, fmt.Errorf("%s.secretRef.key: %q is not a key of a Secret (letters, digits, -, _ and .)", at, r.SecretRef.Key))
		}
		return errs
	case r.File != "":
		if len(r.File) > 4096 || !filepath.IsAbs(r.File) || filepath.Clean(r.File) != r.File {
			return []error{fmt.Errorf("%s.file: use a clean absolute path of at most 4096 bytes", at)}
		}
	default:
		if len(r.Env) > 128 || !credentialEnvironmentName.MatchString(r.Env) {
			return []error{fmt.Errorf("%s.env: %q is not the name of an environment variable", at, r.Env)}
		}
	}
	return nil
}

// validateSinkCommon checks the URL and the CA reference; the shape checks
// the compression against its closed set.
func validateSinkCommon(at, rawURL string, ca *CredentialRef) []error {
	var errs []error
	if rawURL != "" { // the shape reports a missing URL
		// The message never repeats the value: a refused URL is exactly
		// where someone typed a password, and the message is logged.
		if _, err := ParseSinkURL(rawURL); err != nil {
			errs = append(errs, fmt.Errorf("%s.url %w", at, err))
		}
	}
	if ca != nil {
		errs = append(errs, ca.validate(at+".caRef")...)
	}
	return errs
}

// validUsername accepts what HTTP basic authentication can carry.
func validUsername(name string) bool {
	return len(name) <= 128 && !strings.ContainsFunc(name, func(r rune) bool { return r < ' ' || r == 0x7f || r == ':' })
}

// Validate reports everything that keeps the sink from being built; at is
// the path of the object, e.g. querylog.sinks[0].clickhouse.
func (c *QuerylogClickHouse) Validate(at string) []error {
	errs := validateSinkCommon(at, c.URL, c.CARef)
	if !clickHouseIdentifier.MatchString(c.DatabaseName()) {
		errs = append(errs, fmt.Errorf("%s.database: %q is not a name of letters, digits and underscores, at most 63 characters", at, c.Database))
	}
	if !clickHouseIdentifier.MatchString(c.TableName()) {
		errs = append(errs, fmt.Errorf("%s.table: %q is not a name of letters, digits and underscores, at most 63 characters", at, c.Table))
	}
	if !validUsername(c.Username) {
		errs = append(errs, fmt.Errorf("%s.username: use at most 128 characters without a colon or control characters", at))
	}
	if c.PasswordRef != nil {
		if c.Username == "" {
			errs = append(errs, fmt.Errorf("%s.username is required with passwordRef", at))
		}
		errs = append(errs, c.PasswordRef.validate(at+".passwordRef")...)
	}
	return errs
}

// Validate reports everything that keeps the sink from being built; at is
// the path of the object, e.g. querylog.sinks[0].loki.
func (l *QuerylogLoki) Validate(at string) []error {
	errs := validateSinkCommon(at, l.URL, l.CARef)
	if l.Tenant != "" && !lokiTenant.MatchString(l.Tenant) {
		errs = append(errs, fmt.Errorf("%s.tenant: %q is not a Loki tenant ID (letters, digits and !-_.*'(), at most 150 characters)", at, l.Tenant))
	}
	if !validUsername(l.Username) {
		errs = append(errs, fmt.Errorf("%s.username: use at most 128 characters without a colon or control characters", at))
	}
	switch {
	case l.BearerTokenRef != nil && (l.Username != "" || l.PasswordRef != nil):
		errs = append(errs, fmt.Errorf("%s: use either username with passwordRef or bearerTokenRef, not both", at))
	case l.PasswordRef != nil && l.Username == "":
		errs = append(errs, fmt.Errorf("%s.username is required with passwordRef", at))
	case l.Username != "" && l.PasswordRef == nil:
		errs = append(errs, fmt.Errorf("%s.passwordRef is required with username", at))
	}
	if l.PasswordRef != nil {
		errs = append(errs, l.PasswordRef.validate(at+".passwordRef")...)
	}
	if l.BearerTokenRef != nil {
		errs = append(errs, l.BearerTokenRef.validate(at+".bearerTokenRef")...)
	}
	if len(l.Labels) > MaxQuerylogLokiLabels {
		errs = append(errs, fmt.Errorf("%s.labels: %d labels, at most %d are supported", at, len(l.Labels), MaxQuerylogLokiLabels))
	}
	names := make([]string, 0, len(l.Labels))
	for name := range l.Labels {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		value := l.Labels[name]
		switch {
		case !lokiLabelName.MatchString(name) || strings.HasPrefix(name, "__"):
			errs = append(errs, fmt.Errorf("%s.labels: %q is not a label name (letters, digits and underscores, not starting with a digit or two underscores, at most 64 characters)", at, name))
		case name == QuerylogLokiLabelNode || name == QuerylogLokiLabelStatusClass:
			errs = append(errs, fmt.Errorf("%s.labels: %q is set by the sink for every query; choose another name", at, name))
		case value == "" || len(value) > 128 || strings.ContainsFunc(value, func(r rune) bool { return r < ' ' || r == 0x7f }):
			errs = append(errs, fmt.Errorf("%s.labels[%s]: use a value of 1 to 128 characters without control characters", at, name))
		}
	}
	return errs
}

// validateType checks that an entry carries the settings of its type and of
// no other, and the settings themselves.
func (s *QuerylogSink) validateType(at string) []error {
	if s.Type == "" {
		return nil // the shape reports the missing type
	}
	var errs []error
	for _, block := range []struct {
		name string
		set  bool
	}{{QuerylogSinkClickHouse, s.ClickHouse != nil}, {QuerylogSinkLoki, s.Loki != nil}, {QuerylogSinkDnstap, s.Dnstap != nil}} {
		switch {
		case block.set && s.Type != block.name:
			errs = append(errs, fmt.Errorf("%s.%s: these settings belong to a sink of type %s, not %s", at, block.name, block.name, s.Type))
		case !block.set && s.Type == block.name:
			errs = append(errs, fmt.Errorf("%s.%s is required for a sink of type %s", at, block.name, block.name))
		}
	}
	if s.Type == QuerylogSinkClickHouse && s.ClickHouse != nil {
		errs = append(errs, s.ClickHouse.Validate(at+"."+QuerylogSinkClickHouse)...)
	}
	if s.Type == QuerylogSinkLoki && s.Loki != nil {
		errs = append(errs, s.Loki.Validate(at+"."+QuerylogSinkLoki)...)
	}
	if s.Type == QuerylogSinkDnstap && s.Dnstap != nil {
		errs = append(errs, s.Dnstap.Validate(at+"."+QuerylogSinkDnstap)...)
	}
	return errs
}

// credentialRefs lists the references of an entry with their paths.
func (s *QuerylogSink) credentialRefs(at string) map[string]*CredentialRef {
	out := map[string]*CredentialRef{}
	if c := s.ClickHouse; c != nil {
		out[at+".clickhouse.passwordRef"] = c.PasswordRef
		out[at+".clickhouse.caRef"] = c.CARef
	}
	if l := s.Loki; l != nil {
		out[at+".loki.passwordRef"] = l.PasswordRef
		out[at+".loki.bearerTokenRef"] = l.BearerTokenRef
		out[at+".loki.caRef"] = l.CARef
	}
	if d := s.Dnstap; d != nil {
		out[at+".dnstap.caRef"] = d.CARef
		out[at+".dnstap.certRef"] = d.CertRef
		out[at+".dnstap.keyRef"] = d.KeyRef
	}
	return out
}

// validateSinkSecrets refuses a Secret reference where no node could read
// it: Secrets are read through the Kubernetes API, with the server's own
// ServiceAccount, in the namespace it runs in.
func (c *Config) validateSinkSecrets() []error {
	if c.Store.Driver == "kube" {
		return nil
	}
	var errs []error
	for i := range c.Querylog.Sinks {
		refs := c.Querylog.Sinks[i].credentialRefs(fmt.Sprintf("querylog.sinks[%d]", i))
		paths := make([]string, 0, len(refs))
		for path := range refs {
			paths = append(paths, path)
		}
		sort.Strings(paths)
		for _, path := range paths {
			if ref := refs[path]; ref != nil && ref.SecretRef != nil {
				errs = append(errs, fmt.Errorf("%s.secretRef: Secrets are read on Kubernetes only (store.driver kube); use file with credentials.allowedFiles", path))
			}
		}
	}
	return errs
}
