package auditlog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/rest"

	auditv1 "github.com/robertherbaugh/kudurru/api/audit/v1alpha1"
)

// Diagnostic states describe an operational observation, not audit integrity,
// shipping progress, reader selection, or an original canonical serving cut.
const (
	DiagnosticAvailable     = "available"
	DiagnosticOverLimit     = "overLimit"
	DiagnosticMissing       = "missing"
	DiagnosticForbidden     = "forbidden"
	DiagnosticUnavailable   = "unavailable"
	DiagnosticIndeterminate = "indeterminate"

	// RecommendedObjects counts batch objects in one release namespace,
	// not individual events or all namespaces in the Kubernetes cluster.
	RecommendedObjects = 50_000
	probePageSize      = 500
	probeMaxPages      = 101
	probePageBytes     = 2 << 20
	probeTotalBytes    = 64 << 20
	probeTimeout       = 3 * time.Second
	probeTokenBytes    = 4 << 10
)

// Diagnostic contains no batch contents or object metadata. Count is observed
// cardinality in a single namespaced list snapshot. It is exact only on a
// complete scan; OverLimit stops at 50,001 and reports a lower bound.
type Diagnostic struct {
	State     string
	Namespace string
	Count     int
	Exact     bool
}

// Probe uses the caller's guarded Kubernetes transport. It performs no
// discovery, redirects or full-object fallback, and never uses Reader.list
// (which reads annotations for audit history and has different bounds).
type Probe struct {
	client    *http.Client
	endpoint  *url.URL
	namespace string
	scan      chan struct{}
}

// NewProbe does no network acquisition. Authorization and license checks must
// happen before Diagnose, including when the CRD was absent during startup.
func NewProbe(cfg *rest.Config, namespace string) (*Probe, error) {
	if cfg == nil || cfg.Host == "" || namespace == "" || len(validation.IsDNS1123Label(namespace)) != 0 {
		return nil, errors.New("auditlog: invalid diagnostic configuration")
	}
	base, _, err := rest.DefaultServerUrlFor(cfg)
	if err != nil || base.User != nil || base.RawQuery != "" || base.Fragment != "" || base.Scheme != "http" && base.Scheme != "https" {
		return nil, errors.New("auditlog: invalid diagnostic API endpoint")
	}
	// Keep the existing rest.Config transport wrappers, credentials and TLS
	// authority pin. No independent transport or default-client fallback.
	hc, err := rest.HTTPClientFor(rest.CopyConfig(cfg))
	if err != nil {
		return nil, errors.New("auditlog: diagnostic transport unavailable")
	}
	bounded := *hc
	bounded.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Probe{client: &bounded, namespace: namespace, scan: make(chan struct{}, 1),
		endpoint: base.JoinPath("apis", auditv1.GroupVersion.String(), "namespaces", namespace, "auditlogs")}, nil
}

// Diagnose is a bounded, cancelable metadata-only namespace scan. An incomplete
// scan never yields Available. No backend error text or metadata is retained.
func (p *Probe) Diagnose(ctx context.Context) Diagnostic {
	return p.diagnose(ctx, probeTotalBytes)
}

// A smaller private byte budget makes byte exhaustion deterministic in tests
// under race instrumentation without increasing the production scan deadline.
func (p *Probe) diagnose(ctx context.Context, byteBudget int) Diagnostic {
	result := Diagnostic{State: DiagnosticUnavailable, Namespace: p.namespace}
	if ctx == nil || ctx.Err() != nil {
		return result
	}
	// Bound aggregate scan memory/work too. Do not queue a caller behind a
	// different request's observation or detach work from its cancellation.
	select {
	case p.scan <- struct{}{}:
		defer func() { <-p.scan }()
	default:
		return result
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	remainingBytes, continuation, resourceVersion := min(byteBudget, probeTotalBytes), "", ""
	seen := map[string]bool{}
	for range probeMaxPages {
		if ctx.Err() != nil {
			return result
		}
		limit := min(probePageSize, RecommendedObjects+1-result.Count)
		endpoint := *p.endpoint
		query := endpoint.Query()
		query.Set("limit", strconv.Itoa(limit))
		if continuation != "" {
			query.Set("continue", continuation)
		}
		endpoint.RawQuery = query.Encode()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
		if err != nil {
			return result
		}
		req.Header.Set("Accept", "application/json;as=PartialObjectMetadataList;g=meta.k8s.io;v=v1")
		response, err := p.client.Do(req)
		if err != nil {
			return result
		}
		if response.StatusCode != http.StatusOK {
			_ = response.Body.Close() // do not read arbitrary error bodies
			switch response.StatusCode {
			case http.StatusNotFound:
				result.State = DiagnosticMissing
			case http.StatusUnauthorized, http.StatusForbidden:
				result.State = DiagnosticForbidden
			case http.StatusGone:
				result.State = DiagnosticIndeterminate // expired cursor, no restart
			}
			return result
		}
		budget := min(probePageBytes, remainingBytes)
		data, err := io.ReadAll(io.LimitReader(response.Body, int64(budget)+1))
		_ = response.Body.Close()
		if ctx.Err() != nil {
			return result
		}
		if err != nil || len(data) > budget {
			result.State = DiagnosticIndeterminate
			return result
		}
		remainingBytes -= len(data)
		page, err := decodeDiagnosticPage(data, limit)
		if ctx.Err() != nil {
			return result
		}
		if err != nil || resourceVersion != "" && resourceVersion != page.rv {
			result.State = DiagnosticIndeterminate
			return result
		}
		resourceVersion = page.rv
		result.Count += page.count
		if result.Count > RecommendedObjects {
			result.State = DiagnosticOverLimit
			return result
		}
		if page.continuation == "" {
			result.State, result.Exact = DiagnosticAvailable, true
			return result
		}
		if remainingBytes == 0 || seen[page.continuation] {
			result.State = DiagnosticIndeterminate
			return result
		}
		seen[page.continuation] = true
		continuation = page.continuation
	}
	result.State = DiagnosticIndeterminate
	return result
}

type diagnosticPage struct {
	count        int
	continuation string
	rv           string
}

// Decode only the list envelope and bounded item cardinality. Partial metadata
// can itself carry large annotations; the byte bound is enforced BEFORE JSON
// decoding. Item contents are discarded and never enter the diagnostic result.
func decodeDiagnosticPage(data []byte, limit int) (diagnosticPage, error) {
	var page diagnosticPage
	invalid := errors.New("auditlog: invalid metadata page")
	d := json.NewDecoder(bytes.NewReader(data))
	first, err := d.Token()
	if err != nil || first != json.Delim('{') {
		return page, invalid
	}
	seen := map[string]bool{}
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return page, invalid
		}
		field, ok := key.(string)
		if !ok || seen[field] {
			return page, invalid
		}
		seen[field] = true
		switch field {
		case "apiVersion", "kind":
			var value string
			if d.Decode(&value) != nil || field == "apiVersion" && value != "meta.k8s.io/v1" || field == "kind" && value != "PartialObjectMetadataList" {
				return page, invalid
			}
		case "metadata":
			var metadata struct {
				Continue string `json:"continue"`
				RV       string `json:"resourceVersion"`
			}
			if d.Decode(&metadata) != nil || metadata.RV == "" || len(metadata.RV) > 256 || len(metadata.Continue) > probeTokenBytes {
				return page, invalid
			}
			page.rv, page.continuation = metadata.RV, metadata.Continue
		case "items":
			start, err := d.Token()
			if err != nil {
				return page, invalid
			}
			// Kubernetes encodes an empty PartialObjectMetadataList as null.
			// The required items field is still distinct from an absent one.
			if start == nil {
				continue
			}
			if start != json.Delim('[') {
				return page, invalid
			}
			for d.More() {
				if page.count >= limit {
					return page, invalid
				}
				var item struct {
					Metadata *struct{} `json:"metadata"`
				}
				if d.Decode(&item) != nil || item.Metadata == nil {
					return page, invalid
				}
				page.count++
			}
			if end, err := d.Token(); err != nil || end != json.Delim(']') {
				return page, invalid
			}
		default:
			return page, invalid
		}
	}
	if end, err := d.Token(); err != nil || end != json.Delim('}') || !seen["items"] || !seen["metadata"] || !seen["kind"] || !seen["apiVersion"] {
		return page, invalid
	}
	if _, err := d.Token(); err != io.EOF {
		return page, invalid
	}
	return page, nil
}
