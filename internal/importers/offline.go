package importers

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/domainlist"
	"github.com/robertherbaugh/kudurru/internal/zone"
)

// OfflineRequest contains conversion options, never source credentials or paths.
// Conversion reads only the supplied reader; it never opens or fetches sources.
type OfflineRequest struct {
	Namespace           string
	LocalZones          []string
	Nameservers         []string
	NameserverAddresses []string
}

const maxOfflineBytes = 4 << 20
const maxOfflineWarnings = 96

type offlineConverter func(context.Context, OfflineRequest, *offlineMap, *Plan) error

func previewOffline(ctx context.Context, source string, req OfflineRequest, r io.Reader, convert offlineConverter) (*Plan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(offlineReader{ctx, r}, maxOfflineBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("cannot read offline source")
	}
	if len(b) > maxOfflineBytes {
		return nil, errors.New("offline source exceeds 4 MiB")
	}
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	var doc, next yaml.Node
	if dec.Decode(&doc) != nil || len(doc.Content) != 1 {
		return nil, errors.New("offline source must be a valid YAML mapping")
	}
	if dec.Decode(&next) != io.EOF {
		return nil, errors.New("offline source must contain exactly one YAML document")
	}
	nodes := 0
	if err := validateOfflineYAML(ctx, &doc, 0, &nodes); err != nil {
		return nil, err
	}
	root, err := offlineMapping(doc.Content[0])
	if err != nil {
		return nil, err
	}
	if req.Namespace == "" {
		req.Namespace = "default"
	}
	names, err := Nameservers(req.Nameservers)
	if err != nil {
		return nil, err
	}
	addresses, err := NameserverAddresses(req.NameserverAddresses)
	if err != nil {
		return nil, err
	}
	if len(names) > 0 && len(addresses) > 0 {
		return nil, errors.New("choose nameservers or nameserverAddresses, not both")
	}
	req.LocalZones = append([]string{}, req.LocalZones...)
	for i, s := range req.LocalZones {
		canonical, ok := offlineDomain(s)
		if !ok {
			return nil, errors.New("local zones must be valid non-root DNS suffixes")
		}
		req.LocalZones[i] = canonical
	}
	sort.Slice(req.LocalZones, func(i, j int) bool { return len(req.LocalZones[i]) > len(req.LocalZones[j]) })
	p := &Plan{Source: &SourceInfo{Type: source}, Objects: []map[string]any{}, Settings: map[string]any{}, Warnings: []string{
		"Offline conversion is partial. Review every warning and selected item before applying; retain the source backup and test DNS before cutover.",
		"Import replaces objects with the same identities, including Policy/default and upstreams.groups.default. Successful object writes remain on partial failure; settings apply only after all selected objects succeed.",
		"Authentication, secrets, listener privileges, DHCP, query history, runtime clients and source file paths are not imported. No referenced files or network sources are read during conversion.",
	}, nameservers: names, addresses: addresses}
	if err := convert(ctx, req, root, p); err != nil {
		return nil, err
	}
	root.finish(p, "root")
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(p.Objects) > 1000 {
		return nil, errors.New("offline conversion exceeds 1,000 objects")
	}
	seen := map[string]bool{}
	for _, doc := range p.Objects {
		id := ObjectID(doc)
		if seen[id] {
			return nil, errors.New("offline conversion contains duplicate object identities")
		}
		seen[id] = true
	}
	return p, nil
}

type offlineReader struct {
	ctx context.Context
	r   io.Reader
}

func (r offlineReader) Read(b []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(b)
}

func validateOfflineYAML(ctx context.Context, n *yaml.Node, depth int, count *int) error {
	*count++
	if *count > 100000 || depth > 32 {
		return errors.New("offline YAML exceeds structure limits")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if n.Kind == yaml.AliasNode || n.Anchor != "" {
		return errors.New("offline YAML anchors and aliases are not supported")
	}
	switch n.Tag {
	case "", "!!map", "!!seq", "!!str", "!!bool", "!!int", "!!float", "!!null", "!!timestamp":
	default:
		return errors.New("offline YAML contains an unsupported tag")
	}
	if len(n.Value) > 65536 {
		return errors.New("offline YAML scalar exceeds 64 KiB")
	}
	if n.Kind == yaml.MappingNode {
		seen := map[string]bool{}
		for i := 0; i < len(n.Content); i += 2 {
			key := n.Content[i]
			if key.Tag != "!!str" || key.Kind != yaml.ScalarNode || key.Value == "<<" {
				return errors.New("offline YAML mapping keys must be strings; merge keys are not supported")
			}
			if seen[key.Value] {
				return errors.New("offline YAML contains duplicate mapping keys")
			}
			seen[key.Value] = true
		}
	}
	for _, child := range n.Content {
		if err := validateOfflineYAML(ctx, child, depth+1, count); err != nil {
			return err
		}
	}
	return nil
}

type offlineMap struct {
	values map[string]*yaml.Node
	used   map[string]bool
}

func offlineMapping(n *yaml.Node) (*offlineMap, error) {
	m := &offlineMap{map[string]*yaml.Node{}, map[string]bool{}}
	if n == nil {
		return m, nil
	}
	if n.Kind != yaml.MappingNode {
		return nil, errors.New("offline source setting must be a mapping")
	}
	for i := 0; i < len(n.Content); i += 2 {
		m.values[n.Content[i].Value] = n.Content[i+1]
	}
	return m, nil
}
func (m *offlineMap) take(key string) *yaml.Node              { m.used[key] = true; return m.values[key] }
func (m *offlineMap) mapping(key string) (*offlineMap, error) { return offlineMapping(m.take(key)) }
func (m *offlineMap) finish(p *Plan, path string) {
	n := 0
	for key := range m.values {
		if !m.used[key] {
			n++
		}
	}
	if n > 0 {
		p.loss(fmt.Sprintf("%s: %d unsupported settings omitted (values redacted).", path, n))
	}
}
func (p *Plan) loss(message string) {
	if len(p.Warnings) < maxOfflineWarnings {
		p.Warnings = append(p.Warnings, message)
	} else if len(p.Warnings) == maxOfflineWarnings {
		p.Warnings = append(p.Warnings, "Additional conversion losses omitted; loss report reached its 96-message limit.")
	}
}
func offlineString(n *yaml.Node) (string, error) {
	if n == nil {
		return "", nil
	}
	if n.Kind != yaml.ScalarNode || n.Tag != "!!str" {
		return "", errors.New("offline source setting must be a string")
	}
	return n.Value, nil
}
func offlineStrings(n *yaml.Node) ([]string, error) {
	out := []string{}
	if n == nil {
		return out, nil
	}
	if n.Kind != yaml.SequenceNode {
		return nil, errors.New("offline source setting must be a string sequence")
	}
	for _, v := range n.Content {
		s, err := offlineString(v)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}
func offlineBool(n *yaml.Node, def bool) (bool, error) {
	if n == nil {
		return def, nil
	}
	if n.Tag != "!!bool" {
		return false, errors.New("offline source setting must be a boolean")
	}
	v, err := strconv.ParseBool(n.Value)
	if err != nil {
		return false, errors.New("offline source boolean is invalid")
	}
	return v, nil
}
func offlineSeconds(n *yaml.Node, def time.Duration) (time.Duration, error) {
	if n == nil {
		return def, nil
	}
	if n.Tag != "!!int" {
		return 0, errors.New("offline source TTL must be integer seconds")
	}
	i, err := strconv.ParseInt(n.Value, 10, 64)
	if err != nil || i < 0 || i > 2147483647 {
		return 0, errors.New("offline source TTL is outside the supported range")
	}
	return time.Duration(i) * time.Second, nil
}
func offlineDuration(n *yaml.Node, def time.Duration) (time.Duration, error) {
	if n == nil {
		return def, nil
	}
	s, err := offlineString(n)
	if err != nil {
		return 0, err
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 || d > time.Duration(2147483647)*time.Second {
		return 0, errors.New("offline source duration must be positive and within the DNS TTL range")
	}
	return d, nil
}
func offlineKeys(m *offlineMap) []string {
	keys := make([]string, 0, len(m.values))
	for k := range m.values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
func offlineName(source, kind, value string) string {
	h := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%s-%s-%x", source, kind, h[:8])
}
func offlineDomain(s string) (string, bool) {
	s = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), ".")
	ns, err := Nameservers([]string{s})
	if err != nil || len(ns) != 1 {
		return "", false
	}
	return ns[0], true
}
func offlineClient(s string) (public.ClientMatcher, bool) {
	if ip, err := netip.ParseAddr(s); err == nil && ip.Zone() == "" {
		return public.ClientMatcher{IP: ip.Unmap().String()}, true
	}
	if prefix, err := netip.ParsePrefix(s); err == nil {
		if prefix.Bits() == prefix.Addr().BitLen() {
			return public.ClientMatcher{IP: prefix.Addr().Unmap().String()}, true
		}
		return public.ClientMatcher{CIDR: prefix.Masked().String()}, true
	}
	return public.ClientMatcher{}, false
}
func offlinePolicy(p *Plan, ns, name string, spec public.PolicySpec) {
	tm, om := meta("Policy", name, ns)
	p.add(&public.Policy{TypeMeta: tm, ObjectMeta: om, Spec: spec})
}
func offlineGroup(p *Plan, ns, name string, clients []public.ClientMatcher) error {
	if len(clients) > 1000 {
		return errors.New("offline client group exceeds 1,000 clients")
	}
	tm, om := meta("ClientGroup", name, ns)
	p.add(&public.ClientGroup{TypeMeta: tm, ObjectMeta: om, Spec: public.ClientGroupSpec{Clients: clients}})
	return nil
}

// Never echo rejected addresses. Only ordinary credential-free URLs and numeric
// IP resolvers are accepted. Stamps, userinfo, queries and fragments are omitted.
func offlineURL(s string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", false
	}
	return u.String(), true
}
func offlineServer(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "sdns:") || strings.ContainsAny(s, "@?#\r\n\t ") {
		return "", false
	}
	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return "", false
		}
		switch u.Scheme {
		case "https":
			if u.Path != "" && u.Path != "/dns-query" {
				return "", false
			}
		case "udp", "tcp", "tls", "quic":
			if u.Path != "" {
				return "", false
			}
		default:
			return "", false
		}
		host := u.Hostname()
		if _, ok := offlineDomain(host); !ok {
			if ip, err := netip.ParseAddr(host); err != nil || ip.Zone() != "" {
				return "", false
			}
		}
		if u.Port() != "" {
			port, err := strconv.Atoi(u.Port())
			if err != nil || port < 1 || port > 65535 {
				return "", false
			}
		}
		return s, true
	}
	if a, err := netip.ParseAddr(s); err == nil && a.Zone() == "" {
		return a.String(), true
	}
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return "", false
	}
	a, err := netip.ParseAddr(host)
	n, e := strconv.Atoi(port)
	if err != nil || a.Zone() != "" || e != nil || n < 1 || n > 65535 {
		return "", false
	}
	return net.JoinHostPort(a.String(), port), true
}
func offlineEntries(p *Plan, lines []string, path string) []public.ListEntry {
	var entries []public.ListEntry
	seen := map[string]bool{}
	for _, s := range lines {
		s = strings.TrimSpace(s)
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		typ := public.EntryExact
		if strings.HasPrefix(s, "*.") {
			typ = public.EntryWildcard
		} else if strings.HasPrefix(s, "/") && strings.HasSuffix(s, "/") {
			typ = public.EntryRegex
		}
		n, err := domainlist.Normalize(typ, s)
		if err != nil {
			p.loss(path + ": unsupported inline rule omitted (value redacted).")
			continue
		}
		id := string(typ) + "/" + n
		if seen[id] {
			continue
		}
		seen[id] = true
		entries = append(entries, public.ListEntry{Type: typ, Value: n, Origin: public.OriginMigrated})
	}
	return entries
}
func offlineZone(p *Plan, source, ns, name string, lines []string) error {
	im, err := zone.ImportBIND(strings.NewReader(strings.Join(lines, "\n")), name)
	if err != nil {
		return errors.New("offline local records could not be converted")
	}
	if len(im.Warnings) > 0 {
		p.loss("Local records have no source SOA/NS; destination authority defaults apply unless explicit nameservers are selected.")
	}
	im.Warnings = nil
	if err := p.zone(source, ns, name, im); err != nil {
		return errors.New("offline local authority conflicts with imported records or destination nameservers")
	}
	return nil
}
func offlineTTL(d time.Duration) metav1.Duration { return metav1.Duration{Duration: d} }
