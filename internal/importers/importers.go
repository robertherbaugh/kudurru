// Package importers reads foreign DNS APIs and converts their configuration
// into a preview that the normal object and settings writers can apply.
package importers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/domainlist"
	"github.com/robertherbaugh/kudurru/internal/zone"
)

// MigrationRequest contains transient source credentials. Never persist or log it.
type MigrationRequest struct {
	Source             string   `json:"source" enum:"pihole,technitium"`
	URL                string   `json:"url" minLength:"1" doc:"Source web console URL, including any reverse-proxy prefix."`
	Credential         string   `json:"credential" minLength:"1" writeOnly:"true" doc:"Technitium API token or Pi-hole v6 application password."`
	Namespace          string   `json:"namespace,omitempty"`
	InsecureSkipVerify bool     `json:"insecureSkipVerify,omitempty" doc:"Skip source TLS verification for a lab with a self-signed certificate."`
	LocalZones         []string `json:"localZones,omitempty" doc:"Pi-hole local DNS suffixes to make authoritative, for example home.arpa."`
	Nameservers        []string `json:"nameservers,omitempty" maxItems:"16" doc:"Replace imported authoritative NS names and SOA primary with these kudurru DNS names; first name becomes SOA primary. Empty preserves source authority for a later cutover."`
}

// SourceInfo identifies the source without credentials or URL query parameters.
type SourceInfo struct {
	Type string `json:"type" enum:"pihole,technitium"`
	URL  string `json:"url"`
}

// ZoneAuthority preserves the source authority when the preview replaces it.
type ZoneAuthority struct {
	Name        string   `json:"name"`
	Nameservers []string `json:"nameservers"`
	Primary     string   `json:"primary"`
}

// Plan has no source credentials. Objects use the regular apply path;
// Settings is a merge patch through the regular settings path.
type Plan struct {
	Source      *SourceInfo      `json:"source,omitempty"`
	Zones       []ZoneAuthority  `json:"zones,omitempty"`
	Objects     []map[string]any `json:"objects" nullable:"false"`
	Settings    map[string]any   `json:"settings"`
	Warnings    []string         `json:"warnings" nullable:"false"`
	nameservers []string
}

type sourceClient struct {
	base    *url.URL
	http    *http.Client
	headers http.Header
}

const maxResponseBytes = 32 << 20

// Preview reads a live API. Source network access is intentional (including
// private LANs); callers must restrict it to administrators. Redirects are
// refused so credentials cannot be forwarded to another endpoint.
// The REST settings.write gate grants this network access even when a role
// has no other administrator permissions; it is not a network sandbox.
func Preview(ctx context.Context, req MigrationRequest) (*Plan, error) {
	nameservers, err := Nameservers(req.Nameservers)
	if err != nil {
		return nil, err
	}
	if req.Source != "pihole" && req.Source != "technitium" {
		return nil, errors.New("source must be pihole or technitium")
	}
	u, err := url.Parse(strings.TrimRight(req.URL, "/"))
	if err != nil || u == nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("source URL must be HTTP or HTTPS without credentials, query parameters or a fragment")
	}
	if req.Credential == "" {
		return nil, errors.New("a source API token or application password is required")
	}
	if req.Namespace == "" {
		req.Namespace = "default"
	}
	// Accept both console URLs and the API base URL.
	u.Path = strings.TrimSuffix(strings.TrimRight(u.Path, "/"), "/api")
	if req.Source == "pihole" {
		u.Path = strings.TrimSuffix(u.Path, "/admin")
	}
	u.RawPath = ""
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: req.InsecureSkipVerify} //nolint:gosec // explicit source-only opt-in
	defer tr.CloseIdleConnections()
	c := &sourceClient{base: u, headers: make(http.Header), http: &http.Client{
		Transport: tr, Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("source redirects are disabled; use the final source URL")
		},
	}}
	p := &Plan{Objects: []map[string]any{}, Settings: map[string]any{}, Warnings: []string{
		"Import replaces objects with the same names, including Policy/default and upstreams.groups.default. Review existing configuration before importing.",
		"Imports are not transactional: successful object writes remain if another object fails. Settings change only after all objects succeed.",
		"DHCP, query history, users, credentials, certificates, DNSSEC and source-specific extensions are not migrated. Keep the source backup and validate DNS before switching clients.",
	}}
	p.Source = &SourceInfo{Type: req.Source, URL: u.String()}
	p.nameservers = nameservers
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if req.Source == "pihole" {
		err = c.pihole(ctx, req, p)
	} else {
		err = c.technitium(ctx, req, p)
	}
	if err != nil {
		return nil, err
	}
	return p, nil
}

func (c *sourceClient) request(ctx context.Context, method, path string, query url.Values, body any) ([]byte, error) {
	u := *c.base
	u.Path += "/api" + path
	u.RawQuery = query.Encode()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), r)
	if err != nil {
		return nil, fmt.Errorf("source %s: cannot create request", path)
	}
	req.Header = c.headers.Clone()
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.http.Do(req)
	// Do not echo transport errors, URLs or upstream error bodies: any of
	// them can contain a source credential.
	if err != nil {
		return nil, fmt.Errorf("source %s: connection failed (check address, TLS certificate and source availability)", path)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("source %s: HTTP %d", path, res.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("source %s: cannot read response", path)
	}
	if len(b) > maxResponseBytes {
		return nil, fmt.Errorf("source %s: response exceeds 32 MiB", path)
	}
	return b, nil
}

func (c *sourceClient) get(ctx context.Context, path string, out any) error {
	b, err := c.request(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return err
	}
	if json.Unmarshal(b, out) != nil {
		return fmt.Errorf("source %s: invalid JSON response", path)
	}
	return nil
}

func (p *Plan) add(obj any) {
	// Only our own typed objects reach here; round-trip to the generic apply
	// representation and omit fields owned by the destination server.
	b, _ := json.Marshal(obj)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	delete(m, "status")
	delete(m["metadata"].(map[string]any), "creationTimestamp")
	p.Objects = append(p.Objects, m)
}

func meta(kind, name, ns string) (metav1.TypeMeta, metav1.ObjectMeta) {
	return metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: kind}, metav1.ObjectMeta{Name: name, Namespace: ns}
}

func (p *Plan) lists(ns, prefix, mode string, sources []v1alpha1.ListSource) []string {
	var refs []string
	for i := 0; i < len(sources); {
		end, size := i, 0
		for end < len(sources) && end-i < 256 {
			b, _ := json.Marshal(sources[end])
			if end > i && size+len(b) > 1<<20 {
				break
			}
			size += len(b)
			end++
		}
		name := prefix
		if len(refs) > 0 {
			name = fmt.Sprintf("%s-%d", prefix, len(refs)+1)
		}
		tm, om := meta("BlockList", name, ns)
		p.add(&v1alpha1.BlockList{TypeMeta: tm, ObjectMeta: om, Spec: v1alpha1.BlockListSpec{Mode: v1alpha1.ListMode(mode), Sources: sources[i:end]}})
		refs = append(refs, name)
		i = end
	}
	return refs
}

func entrySources(entries []v1alpha1.ListEntry) []v1alpha1.ListSource {
	var sources []v1alpha1.ListSource
	for i := 0; i < len(entries); {
		end, size := i, 0
		for end < len(entries) && end-i < 10000 {
			b, _ := json.Marshal(entries[end])
			if end > i && size+len(b) > 512<<10 {
				break
			}
			size += len(b)
			end++
		}
		sources = append(sources, v1alpha1.ListSource{Name: fmt.Sprintf("domains-%d", len(sources)+1), Entries: entries[i:end]})
		i = end
	}
	return sources
}

func (p *Plan) entry(value string, typ v1alpha1.EntryType, enabled bool, comment string) (v1alpha1.ListEntry, bool) {
	if typ == v1alpha1.EntryRegex && strings.Contains(value, ";") {
		p.Warnings = append(p.Warnings, fmt.Sprintf("Skipped regex %q: Pi-hole extensions require manual conversion.", value))
		return v1alpha1.ListEntry{}, false
	}
	n, err := domainlist.Normalize(typ, value)
	if err != nil {
		p.Warnings = append(p.Warnings, fmt.Sprintf("Skipped domain rule %q: not supported by kudurru.", value))
		return v1alpha1.ListEntry{}, false
	}
	return v1alpha1.ListEntry{Value: n, Type: typ, Enabled: &enabled, Comment: comment, Origin: v1alpha1.OriginMigrated}, true
}

var nonName = regexp.MustCompile(`[^a-z0-9-]+`)

func zoneName(source, domain string) string {
	slug := strings.Trim(nonName.ReplaceAllString(strings.ToLower(domain), "-"), "-")
	if slug == "" {
		slug = "root"
	}
	if len(slug) > 35 {
		slug = strings.TrimRight(slug[:35], "-")
	}
	h := sha256.Sum256([]byte(domain))
	return fmt.Sprintf("%s-%s-%x", source, slug, h[:4])
}

func (p *Plan) zone(source, ns, name string, im *zone.Imported) {
	id := zoneName(source, name)
	primary := ""
	if im.Spec.SOA != nil {
		primary = im.Spec.SOA.MName
	}
	p.Zones = append(p.Zones, ZoneAuthority{Name: id, Nameservers: append([]string{}, im.Spec.Nameservers...), Primary: primary})
	if len(p.nameservers) > 0 {
		im.Spec.Nameservers = append([]string(nil), p.nameservers...)
		if im.Spec.SOA == nil {
			im.Spec.SOA = &v1alpha1.SOASpec{}
		} else {
			soa := *im.Spec.SOA
			im.Spec.SOA = &soa
		}
		im.Spec.SOA.MName = p.nameservers[0]
	}
	dz, sets := im.Objects(id, ns, id)
	p.add(dz)
	for _, set := range sets {
		p.add(set)
	}
	for _, w := range im.Warnings {
		p.Warnings = append(p.Warnings, name+": "+w)
	}
}
