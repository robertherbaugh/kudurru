package iammigrate

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/iamlegacy"
	"github.com/robertherbaugh/kudurru/internal/store"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type source struct {
	secret *corev1.Secret
	obj    store.Object
	admin  *bool
}

var sourceKinds = map[string]bool{"ServiceAccount": true, "RoleBinding": true, "IdentityProvider": true, "LocalUser": true, "APIToken": true, "PrincipalState": true}

func sourceKey(s *source) key {
	return key{Kind: string(store.KindOf(s.obj)), Namespace: s.obj.GetNamespace(), Name: s.obj.GetName()}
}

func sourceDigest(s *corev1.Secret) string {
	return digest(struct {
		UID                 string
		RV                  string
		Created             metav1.Time
		Labels, Annotations map[string]string
		Type                corev1.SecretType
		Data                map[string][]byte
	}{string(s.UID), s.ResourceVersion, s.CreationTimestamp, s.Labels, s.Annotations, s.Type, s.Data})
}

func (m *Migrator) decodeSource(sec *corev1.Secret) (*source, error) {
	kind := sec.Labels[kindLabel]
	raw := sec.Data["object"]
	bad := func() (*source, error) {
		return nil, problem("invalid-source", kind, m.opts.Namespace, sec.Name, "decode")
	}
	if !sourceKinds[kind] || sec.Type != internalType || sec.Namespace != m.opts.Namespace || sec.UID == "" || len(sec.UID) > 512 || sec.ResourceVersion == "" || sec.CreationTimestamp.IsZero() || sec.DeletionTimestamp != nil || !boundedJSON(raw, m.opts.Limits.ObjectBytes) || len(sec.Data) != 1 {
		return bad()
	}
	archiveBytes, err := json.Marshal(sec)
	if err != nil || len(archiveBytes) > 768<<10 {
		return nil, problem("bounds-exceeded", kind, m.opts.Namespace, sec.Name, "source")
	}
	var obj store.Object
	var admin *bool
	if _, public := publicResources[kind]; public {
		if kind == "ServiceAccount" {
			var doc map[string]json.RawMessage
			var spec map[string]json.RawMessage
			if json.Unmarshal(raw, &doc) != nil || json.Unmarshal(doc["spec"], &spec) != nil || spec == nil {
				return bad()
			}
			if v, ok := spec["builtinAdmin"]; ok {
				var value bool
				if string(v) == "null" || json.Unmarshal(v, &value) != nil {
					return bad()
				}
				admin = &value
				delete(spec, "builtinAdmin")
				doc["spec"], _ = json.Marshal(spec)
				raw, _ = json.Marshal(doc)
			}
		}
		var err error
		obj, err = iamlegacy.Convert(kind, raw)
		if err != nil {
			return bad()
		}
	} else {
		switch kind {
		case "LocalUser":
			obj = &internalv1.LocalUser{}
		case "APIToken":
			obj = &internalv1.APIToken{}
		case "PrincipalState":
			obj = &internalv1.PrincipalState{}
		}
		if !strict(raw, obj, m.opts.Limits.ObjectBytes) {
			return bad()
		}
		gvk := obj.GetObjectKind().GroupVersionKind()
		if gvk.Kind != "" && gvk.Kind != kind || gvk.Version != "" && gvk.GroupVersion() != publicv1.GroupVersion {
			return bad()
		}
		obj.GetObjectKind().SetGroupVersionKind(publicv1.GroupVersion.WithKind(kind))
		if obj.(interface{ Validate() error }).Validate() != nil {
			return bad()
		}
	}
	if sec.Name != secretName(kind, obj.GetNamespace(), obj.GetName()) || sec.Annotations[nameAnnotation] != obj.GetName() || sec.Labels[namespaceLabel] != obj.GetNamespace() {
		return bad()
	}
	obj.SetUID(sec.UID)
	obj.SetResourceVersion(sec.ResourceVersion)
	obj.SetCreationTimestamp(sec.CreationTimestamp)
	if obj.GetDeletionTimestamp() != nil {
		return bad()
	}
	if idp, ok := obj.(*publicv1.IdentityProvider); ok && idp.Spec.ClientSecretRef.File != "" && !m.files[idp.Spec.ClientSecretRef.File] {
		return nil, problem("credentials-denied", kind, obj.GetNamespace(), obj.GetName(), "convert")
	}
	if _, public := publicResources[kind]; public {
		if len(obj.GetOwnerReferences()) > 0 || len(obj.GetFinalizers()) > 0 {
			return nil, problem("unsupported-lifecycle", kind, obj.GetNamespace(), obj.GetName(), "convert")
		}
		annotations := obj.GetAnnotations()
		delete(annotations, "kubectl.kubernetes.io/last-applied-configuration")
		for _, metadata := range []map[string]string{annotations, obj.GetLabels()} {
			for k, v := range metadata {
				if strings.Contains(lower(k), "password") || strings.Contains(lower(k), "secrethash") || strings.Contains(lower(k), "builtinadmin") || strings.Contains(v, `"secretHash"`) || strings.Contains(v, `"passwordHash"`) || strings.Contains(v, `"builtinAdmin"`) {
					return bad()
				}
			}
		}
		obj.SetAnnotations(annotations)
		// The old Secret generation is not the newly created CRD generation.
		if idp, ok := obj.(*publicv1.IdentityProvider); ok {
			for i := range idp.Status.Conditions {
				idp.Status.Conditions[i].ObservedGeneration = 0
			}
		}
	}
	return &source{secret: sec.DeepCopy(), obj: obj, admin: admin}, nil
}

func (m *Migrator) sources(ctx context.Context, ignored map[string]bool) ([]*source, error) {
	return m.listSources(ctx, ignored, kindLabel+" in (APIToken,IdentityProvider,LocalUser,PrincipalState,RoleBinding,ServiceAccount)")
}

func (m *Migrator) listSources(ctx context.Context, ignored map[string]bool, selector string) ([]*source, error) {
	var result []*source
	cursor := ""
	total := 0
	seen := map[key]bool{}
	pages := map[string]bool{}
	for {
		rctx, cancel := m.request(ctx)
		list, err := m.core.CoreV1().Secrets(m.opts.Namespace).List(rctx, metav1.ListOptions{LabelSelector: selector, Limit: int64(m.opts.Limits.PageSize), Continue: cursor})
		cancel()
		if err != nil {
			return nil, problem("api-unavailable", "Secret", m.opts.Namespace, "", "list")
		}
		for i := range list.Items {
			sec := &list.Items[i]
			if !sourceKinds[sec.Labels[kindLabel]] || ignored[sec.Name] {
				continue
			}
			s, err := m.decodeSource(sec)
			if err != nil {
				return nil, err
			}
			k := sourceKey(s)
			if seen[k] {
				return nil, problem("duplicate-source", k.Kind, k.Namespace, k.Name, "list")
			}
			seen[k] = true
			total += len(sec.Data["object"])
			result = append(result, s)
			if len(result) > m.opts.Limits.Objects || total > m.opts.Limits.TotalBytes {
				return nil, problem("bounds-exceeded", "Secret", m.opts.Namespace, "", "list")
			}
		}
		cursor = list.Continue
		if cursor == "" {
			break
		}
		if pages[cursor] {
			return nil, problem("invalid-pagination", "Secret", m.opts.Namespace, "", "list")
		}
		pages[cursor] = true
	}
	sort.Slice(result, func(i, j int) bool {
		a, b := sourceKey(result[i]), sourceKey(result[j])
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Name < b.Name
	})
	return result, nil
}
