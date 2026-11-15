package iammigrate

import (
	"context"
	"encoding/json"
	"strings"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Plan is an opaque bounded source capture. Its formatted/JSON forms reveal counts only.
type Plan struct {
	namespace             string
	cluster, namespaceUID types.UID
	sources               []*source
	counts                Counts
	admins                map[key]bool
}

func (p *Plan) String() string {
	if p == nil {
		return "IAM migration plan <nil>"
	}
	return "IAM migration plan (private)"
}

// GoString prevents %#v from exposing private source contents.
func (p *Plan) GoString() string { return p.String() }

// MarshalJSON exposes only public diagnostic counts.
func (p *Plan) MarshalJSON() ([]byte, error) {
	if p == nil {
		return []byte("null"), nil
	}
	return json.Marshal(p.counts)
}

// Counts returns safe diagnostic counts.
func (p *Plan) Counts() Counts {
	if p == nil {
		return Counts{}
	}
	return p.counts
}

func (m *Migrator) identities(ctx context.Context) (types.UID, types.UID, error) {
	rctx, cancel := m.request(ctx)
	cluster, err := m.core.CoreV1().Namespaces().Get(rctx, "kube-system", metav1.GetOptions{})
	cancel()
	if err != nil || cluster.UID == "" {
		return "", "", problem("api-unavailable", "Namespace", "", "kube-system", "identity")
	}
	rctx, cancel = m.request(ctx)
	ns, err := m.core.CoreV1().Namespaces().Get(rctx, m.opts.Namespace, metav1.GetOptions{})
	cancel()
	if err != nil || ns.UID == "" || ns.DeletionTimestamp != nil {
		return "", "", problem("api-unavailable", "Namespace", "", m.opts.Namespace, "identity")
	}
	return cluster.UID, ns.UID, nil
}

// Plan validates all sources, authority and target absence without writing.
func (m *Migrator) Plan(ctx context.Context) (*Plan, error) {
	c, n, err := m.identities(ctx)
	if err != nil {
		return nil, err
	}
	sources, err := m.sources(ctx, nil)
	if err != nil {
		return nil, err
	}
	p, err := m.makePlan(c, n, sources)
	if err != nil {
		return nil, err
	}
	// Discovery verifies scope as well as the resource names; all calls are bounded.
	rest := m.core.Discovery().RESTClient()
	if rest == nil {
		return nil, problem("api-unavailable", "CRD", "", "", "discovery")
	}
	var resources metav1.APIResourceList
	rctx, cancel := m.request(ctx)
	err = rest.Get().AbsPath("/apis/kudurru.io/v1alpha1").Do(rctx).Into(&resources)
	cancel()
	if err != nil {
		return nil, problem("api-unavailable", "CRD", "", "", "discovery")
	}
	for kind, gvr := range publicResources {
		found := false
		for _, resource := range resources.APIResources {
			if resource.Name == gvr.Resource && resource.Kind == kind && resource.Namespaced == (kind != "IdentityProvider") {
				found = true
			}
		}
		if !found {
			return nil, problem("incompatible-schema", kind, "", "", "discovery")
		}
	}
	for _, s := range sources {
		k := sourceKey(s)
		if _, ok := publicResources[k.Kind]; !ok {
			continue
		}
		if k.Namespace != "" {
			rctx, cancel = m.request(ctx)
			_, err = m.core.CoreV1().Namespaces().Get(rctx, k.Namespace, metav1.GetOptions{})
			cancel()
			if err != nil {
				return nil, problem("api-unavailable", "Namespace", "", k.Namespace, "target")
			}
		}
		existing, err := m.getPublic(ctx, k)
		if err != nil {
			return nil, err
		}
		if existing != nil {
			return nil, problem("target-conflict", k.Kind, k.Namespace, k.Name, "plan")
		}
	}
	return p, nil
}

func (m *Migrator) makePlan(c, n types.UID, sources []*source) (*Plan, error) {
	if len(sources) > m.opts.Limits.Objects {
		return nil, problem("bounds-exceeded", "Secret", m.opts.Namespace, "", "plan")
	}
	validated := make([]*source, 0, len(sources))
	total := 0
	for _, s := range sources {
		copySource, err := m.decodeSource(s.secret)
		if err != nil {
			return nil, err
		}
		total += len(s.secret.Data["object"])
		if total > m.opts.Limits.TotalBytes {
			return nil, problem("bounds-exceeded", "Secret", m.opts.Namespace, "", "plan")
		}
		validated = append(validated, copySource)
	}
	sources = validated
	p := &Plan{namespace: m.opts.Namespace, cluster: c, namespaceUID: n, sources: sources, admins: map[key]bool{}}
	byKey := map[key]*source{}
	for _, s := range sources {
		k := sourceKey(s)
		byKey[k] = s
		p.counts.Sources++
		if _, ok := publicResources[k.Kind]; ok {
			p.counts.Public++
		}
		if s.admin != nil {
			p.admins[k] = *s.admin
		}
	}
	for _, s := range sources {
		ps, ok := s.obj.(*internalv1.PrincipalState)
		if !ok {
			continue
		}
		owner := ps.Spec.Owner
		k := key{Kind: "ServiceAccount", Namespace: owner.Namespace, Name: owner.Name}
		sa := byKey[k]
		if sa == nil || string(sa.obj.GetUID()) != owner.UID {
			return nil, problem("private-conflict", k.Kind, k.Namespace, k.Name, "authority")
		}
		if sa.admin != nil && *sa.admin != ps.Spec.BuiltinAdmin {
			return nil, problem("private-conflict", k.Kind, k.Namespace, k.Name, "authority")
		}
		p.admins[k] = ps.Spec.BuiltinAdmin
	}
	for _, admin := range p.admins {
		if admin {
			p.counts.Authorities++
		}
	}
	// Estimate the final representation with the largest accepted UID receipts.
	j := p.journal(strings.Repeat("i", 36), strings.Repeat("r", 36))
	targets := map[key]types.UID{}
	for i := range j.Public {
		j.Public[i].UID = types.UID(strings.Repeat("u", 512))
		j.Public[i].Intent = true
		j.Public[i].StatusDone = true
		targets[j.Public[i].Key] = j.Public[i].UID
	}
	for k, admin := range p.admins {
		if admin {
			j.Private = append(j.Private, receipt{Key: key{Kind: "PrincipalState", Name: internalv1.PrincipalStateName(accountOwner(k, targets[k]))}, Digest: strings.Repeat("d", 64), Intent: true, UID: types.UID(strings.Repeat("u", 512)), StatusDone: true})
		}
	}
	credentials, bound, unbound, err := p.credentials(targets)
	if err != nil {
		return nil, err
	}
	j.Credentials = credentials
	p.counts.BoundTokens = bound
	p.counts.UnboundTokens = unbound
	j.Counts = p.counts
	j.Completion = strings.Repeat("d", 64)
	data, err := json.Marshal(j)
	if err != nil || len(data) > m.opts.Limits.JournalBytes {
		return nil, problem("bounds-exceeded", "Secret", m.opts.Namespace, JournalName, "plan")
	}
	return p, nil
}

func (p *Plan) journal(id, owner string) *journal {
	j := &journal{Version: journalVersion, Converter: 1, Cluster: p.cluster, NamespaceUID: p.namespaceUID, Namespace: p.namespace, ID: id, Owner: owner, State: Running, Counts: p.counts}
	for _, s := range p.sources {
		k := sourceKey(s)
		j.Sources = append(j.Sources, sourceRecord{Key: k, Secret: s.secret.Name, UID: s.secret.UID, RV: s.secret.ResourceVersion, Digest: sourceDigest(s.secret), Archive: "kudurru-iam-source-" + digest([]string{id, s.secret.Name})[:48]})
		if _, ok := publicResources[k.Kind]; ok {
			j.Public = append(j.Public, receipt{Key: k, Digest: publicDigest(s.obj)})
		}
	}
	j.Manifest = digest(j.Sources)
	return j
}

func accountOwner(k key, uid types.UID) internalv1.TokenOwner {
	return internalv1.TokenOwner{Kind: internalv1.OwnerServiceAccount, Name: k.Name, Namespace: k.Namespace, ID: "sa:" + k.Namespace + "/" + k.Name, Provider: "kudurru", UID: string(uid)}
}

func publicDigest(obj store.Object) string {
	// Public identity/status are checked separately; resourceVersion/time are local.
	return digest(struct {
		Key                 key
		Labels, Annotations map[string]string
		Spec                any
	}{key{string(store.KindOf(obj)), obj.GetNamespace(), obj.GetName()}, obj.GetLabels(), obj.GetAnnotations(), specOf(obj)})
}
func specOf(obj store.Object) any {
	data, _ := json.Marshal(obj)
	var doc map[string]json.RawMessage
	_ = json.Unmarshal(data, &doc)
	return doc["spec"]
}
