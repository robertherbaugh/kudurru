package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/apiclient"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/kube"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// KubeOptions explicitly selects a host kubeconfig and its context. API endpoint
// credentials never participate in this transport.
type KubeOptions struct {
	ConfigPath, Context, Namespace string
	Timeout                        time.Duration
	Getenv                         func(string) string
}

// KubeObjects is the generic CLI's bounded direct CRD transport. It has no server
// runtime, background watches, private kinds or arbitrary REST request surface.
type KubeObjects struct {
	direct *kube.Direct
	scheme *runtime.Scheme
}

// NewKubeObjects loads only operator-selected kubeconfig credentials, preserving
// their TLS, exec-auth and impersonation settings. It does not contact a cluster.
func NewKubeObjects(o KubeOptions) (*KubeObjects, string, error) {
	if o.Getenv == nil || o.Timeout <= 0 || o.Timeout > 5*time.Minute {
		return nil, "", errors.New("kubernetes transport requires environment access and request-timeout in (0,5m]")
	}
	rules := &clientcmd.ClientConfigLoadingRules{ExplicitPath: o.ConfigPath}
	if o.ConfigPath == "" {
		if value := o.Getenv(clientcmd.RecommendedConfigPathEnvVar); value != "" {
			rules.Precedence = filepath.SplitList(value)
		} else {
			home := o.Getenv("HOME")
			if home == "" {
				return nil, "", errors.New("set --kubeconfig or KUBECONFIG when HOME is unavailable")
			}
			rules.Precedence = []string{filepath.Join(home, ".kube", "config")}
		}
	}
	overrides := &clientcmd.ConfigOverrides{CurrentContext: o.Context}
	if o.Namespace != "" {
		overrides.Context.Namespace = o.Namespace
	}
	loader := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides)
	raw, err := loader.RawConfig()
	if err != nil {
		return nil, "", fmt.Errorf("kubernetes kubeconfig: %w", err)
	}
	selected := o.Context
	if selected == "" {
		selected = raw.CurrentContext
	}
	if selected == "" || raw.Contexts[selected] == nil {
		return nil, "", errors.New("kubernetes context is missing; choose an exact kubeconfig context")
	}
	cfg, err := loader.ClientConfig()
	if err != nil {
		return nil, "", fmt.Errorf("kubernetes kubeconfig: %w", err)
	}
	ns, _, err := loader.Namespace()
	if err != nil {
		return nil, "", fmt.Errorf("kubernetes namespace: %w", err)
	}
	cfg = rest.CopyConfig(cfg)
	cfg.Timeout = o.Timeout
	cfg.UserAgent = "kudurru-cli/direct-crd"
	cfg.Wrap(func(rt http.RoundTripper) http.RoundTripper { return kubeResponseBound{next: rt} })
	scheme := runtime.NewScheme()
	if err := public.AddToScheme(scheme); err != nil {
		return nil, "", err
	}
	direct, err := kube.NewDirect(cfg, kube.Options{Namespace: ns, Scheme: scheme, StartupTimeout: o.Timeout, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		return nil, "", fmt.Errorf("kubernetes transport: %w", err)
	}
	return &KubeObjects{direct: direct, scheme: scheme}, ns, nil
}

// Close releases the direct CRUD transport.
func (c *KubeObjects) Close() error { return c.direct.Close() }

func (c *KubeObjects) object(resource string) (store.Object, Kind, error) {
	k, err := LookupKind(resource)
	if err != nil {
		return nil, k, err
	}
	value, err := c.scheme.New(public.GroupVersion.WithKind(k.Kind))
	if err != nil {
		return nil, k, err
	}
	obj := value.(store.Object)
	obj.GetObjectKind().SetGroupVersionKind(public.GroupVersion.WithKind(k.Kind))
	return obj, k, nil
}

// GetObject reads exactly one supported CRD under the selected credentials.
func (c *KubeObjects) GetObject(ctx context.Context, resource, namespace, name string) (json.RawMessage, error) {
	obj, k, err := c.object(resource)
	if err != nil {
		return nil, err
	}
	if !k.Namespaced && namespace != "" {
		return nil, errors.New("cluster-scoped kind cannot select a namespace")
	}
	err = c.direct.Get(ctx, store.Key{Kind: store.Kind(k.Kind), Namespace: namespace, Name: name}, obj)
	if err != nil {
		return nil, kubeError(err)
	}
	return json.Marshal(obj)
}

// ListObjects follows at most 200 pages, with 10,000-object/64MiB total bounds.
// A failed/expired continuation never returns a partially successful listing.
func (c *KubeObjects) ListObjects(ctx context.Context, resource string, opts apiclient.ListOptions) ([]json.RawMessage, error) {
	_, k, err := c.object(resource)
	if err != nil {
		return nil, err
	}
	if !k.Namespaced && opts.Namespace != "" {
		return nil, errors.New("cluster-scoped kind cannot select a namespace")
	}
	limit := opts.PageSize
	if limit <= 0 {
		limit = 200
	}
	if limit > 500 {
		limit = 500
	}
	var items []json.RawMessage
	cursor := ""
	seen := map[string]bool{}
	size := 0
	for range 200 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		page, err := c.direct.List(ctx, store.Kind(k.Kind), store.ListOptions{Namespace: opts.Namespace, Limit: limit, Continue: cursor}, opts.LabelSelector)
		if err != nil {
			return nil, kubeError(err)
		}
		for _, obj := range page.Items {
			raw, err := json.Marshal(obj)
			if err != nil {
				return nil, err
			}
			size += len(raw)
			if len(items) >= 10000 || size > 64<<20 {
				return nil, errors.New("kubernetes listing exceeds CLI bounds; narrow namespace or selector")
			}
			items = append(items, raw)
		}
		if page.Continue == "" {
			return items, nil
		}
		if seen[page.Continue] {
			return nil, errors.New("kubernetes repeated a continuation cursor")
		}
		seen[page.Continue] = true
		cursor = page.Continue
	}
	return nil, errors.New("kubernetes listing exceeds 200 pages; narrow namespace or selector")
}

// Apply uses original absence or resource-version evidence and the driver's
// manager-api ownership checks. Batch outcomes are independent, never atomic.
func (c *KubeObjects) Apply(ctx context.Context, docs []json.RawMessage) ([]apiclient.ApplyResult, error) {
	if len(docs) > 1000 {
		return nil, errors.New("direct apply accepts at most 1000 documents")
	}
	out := make([]apiclient.ApplyResult, 0, len(docs))
	for _, raw := range docs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		result := c.applyOne(ctx, raw)
		out = append(out, result)
	}
	return out, nil
}

func (c *KubeObjects) applyOne(ctx context.Context, raw json.RawMessage) (result apiclient.ApplyResult) {
	fail := func(err error) apiclient.ApplyResult {
		result.Result = "error"
		result.Error = err.Error()
		var e *apiclient.Error
		if errors.As(err, &e) {
			result.ErrorKey, result.Hint = e.Key, e.Hint
		}
		return result
	}
	if len(raw) > store.MaxObjectBytes {
		return fail(kubeError(store.ErrTooLarge))
	}
	meta, err := DecodeMeta(raw)
	if err != nil {
		return fail(errors.New("invalid object metadata"))
	}
	result.Kind, result.Namespace, result.Name = meta.Kind, meta.Metadata.Namespace, meta.Metadata.Name
	if meta.APIVersion != public.GroupVersion.String() {
		return fail(errors.New("direct apply requires kudurru.io/v1alpha1"))
	}
	obj, k, err := c.object(meta.Kind)
	if err != nil {
		return fail(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(obj); err != nil {
		return fail(fmt.Errorf("invalid %s document: %w", k.Kind, err))
	}
	if decoder.Decode(new(any)) != io.EOF {
		return fail(errors.New("direct apply requires one complete JSON object per document"))
	}
	if obj.GetName() == "" || !k.Namespaced && obj.GetNamespace() != "" || k.Namespaced && obj.GetNamespace() == "" {
		return fail(errors.New("metadata name/namespace does not match the CRD scope"))
	}
	current, _, err := c.object(meta.Kind)
	if err != nil {
		return fail(err)
	}
	err = c.direct.Get(ctx, store.KeyOf(obj), current)
	opts := store.ApplyOptions{Manager: store.ManagerAPI}
	switch {
	case errors.Is(err, store.ErrNotFound):
		opts.CreateOnly = true
		result.Result = "created"
	case err != nil:
		return fail(kubeError(err))
	default:
		opts.ExpectedResourceVersion = current.GetResourceVersion()
		result.Result = "updated"
	}
	// An explicit manifest version is a precondition, never silently replaced.
	if rv := obj.GetResourceVersion(); rv != "" {
		opts.ExpectedResourceVersion = rv
		opts.CreateOnly = false
	}
	updated, err := c.direct.Apply(ctx, obj, opts)
	if err != nil {
		return fail(kubeError(err))
	}
	result.ResourceVersion = updated.GetResourceVersion()
	return result
}

// DeleteObject preserves ownership and original UID/version preconditions.
func (c *KubeObjects) DeleteObject(ctx context.Context, resource, namespace, name string) error {
	_, k, err := c.object(resource)
	if err != nil {
		return err
	}
	if !k.Namespaced && namespace != "" {
		return errors.New("cluster-scoped kind cannot select a namespace")
	}
	return kubeError(c.direct.Delete(ctx, store.Key{Kind: store.Kind(k.Kind), Namespace: namespace, Name: name}, store.DeleteOptions{Manager: store.ManagerAPI}))
}

// KubernetesError distinguishes direct-cluster failures for actionable CLI hints.
type KubernetesError struct {
	Cause error
	API   *apiclient.Error
}

func (e *KubernetesError) Error() string {
	if e.API != nil {
		return "kubernetes: " + e.API.Error()
	}
	return "kubernetes: " + e.Cause.Error()
}
func (e *KubernetesError) Unwrap() []error {
	if e.API != nil {
		return []error{e.Cause, e.API}
	}
	return []error{e.Cause}
}

func kubeError(err error) error {
	if err == nil {
		return nil
	}
	key, code := "kubernetes", http.StatusBadGateway
	hint := ""
	var owner *store.ErrManagedBy
	switch {
	case errors.Is(err, store.ErrNotFound):
		key, code = "not_found", http.StatusNotFound
	case errors.Is(err, store.ErrConflict), apierrors.IsConflict(err):
		key, code = "conflict", http.StatusConflict
	case errors.As(err, &owner):
		key, code, hint = "managed_by", http.StatusConflict, owner.Owner
	case errors.Is(err, store.ErrTooLarge):
		key, code = "too_large", http.StatusRequestEntityTooLarge
	case apierrors.IsUnauthorized(err):
		key, code = "unauthenticated", http.StatusUnauthorized
	case apierrors.IsForbidden(err):
		key, code = "forbidden", http.StatusForbidden
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		key, code = "invalid", http.StatusBadRequest
	}
	return &KubernetesError{Cause: err, API: &apiclient.Error{StatusCode: code, Key: key, Message: err.Error(), Hint: hint}}
}

// Bound decoded response input even if a source ignores the requested page size.
type kubeResponseBound struct{ next http.RoundTripper }

func (t kubeResponseBound) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(req)
	if err != nil {
		return resp, err
	}
	resp.Body = &kubeBoundBody{ReadCloser: resp.Body, remaining: 16 << 20}
	return resp, nil
}

type kubeBoundBody struct {
	io.ReadCloser
	remaining int64
}

func (b *kubeBoundBody) Read(p []byte) (int, error) {
	if b.remaining == 0 {
		var one [1]byte
		n, err := b.ReadCloser.Read(one[:])
		if n != 0 {
			return 0, errors.New("kubernetes response exceeds 16MiB")
		}
		return 0, err
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.ReadCloser.Read(p)
	b.remaining -= int64(n)
	return n, err
}
