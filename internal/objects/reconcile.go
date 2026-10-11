package objects

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sort"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
)

// filePrefix is the managedBy prefix of file-managed objects
// (store.ManagerFile).
const filePrefix = "file:"

// listPageSize bounds one List call while looking for orphans.
const listPageSize = 500

// scheme decodes kudurru.io/v1alpha1 objects into their Go types.
var scheme = func() *runtime.Scheme {
	s := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(s); err != nil {
		panic(err)
	}
	return s
}()

// isFileManager reports whether a managedBy value names a file.
func isFileManager(owner string) bool { return strings.HasPrefix(owner, filePrefix) }

// source is one decoded source in apply order.
type source struct {
	name string
	decoded
}

// reconciler applies decoded documents to a store. The loader runs it in
// file mode (take over objects that moved between files, delete orphans);
// ApplyFile runs it in plain mode (apply only).
type reconciler struct {
	st        store.Store
	v         *validator.Validator
	defaultNS string
	// manager maps a source name to the ApplyOptions.Manager of its objects.
	manager func(source string) string
	// fileMode enables the loader semantics: objects owned by a file that no
	// longer declares them are taken over (moved) or deleted.
	fileMode bool
	log      *slog.Logger
}

// item is one identified document.
type item struct {
	doc document
	key store.Key
	gvk schema.GroupVersionKind
}

// pass is the state of one reconcile run.
type pass struct {
	r   *reconciler
	rep Report
	// declared maps every identified key to the source of its winning
	// declaration; those objects are never deleted as orphans, even when the
	// declaration is invalid (the last good object is kept).
	declared map[store.Key]string
	// brokenSources and brokenPrefixes name sources that could not be read
	// completely; objects they own are neither deleted nor taken over.
	brokenSources  map[string]bool
	brokenPrefixes []string
}

// run reconciles srcs into the store and returns the report.
func (r *reconciler) run(ctx context.Context, srcs []source, brokenPrefixes []string, readDiags []Diagnosis) Report {
	p := &pass{
		r:              r,
		rep:            Report{Started: time.Now()},
		declared:       map[store.Key]string{},
		brokenSources:  map[string]bool{},
		brokenPrefixes: brokenPrefixes,
	}
	p.rep.Diagnoses = append(p.rep.Diagnoses, readDiags...)

	var items []item
	for _, s := range srcs {
		p.rep.Sources = append(p.rep.Sources, s.name)
		p.rep.Diagnoses = append(p.rep.Diagnoses, s.diags...)
		if s.broken {
			p.brokenSources[s.name] = true
		}
		for _, d := range s.docs {
			it, ok := p.identify(d)
			if !ok {
				p.brokenSources[s.name] = true
				continue
			}
			p.rep.Objects++
			if first, dup := p.declared[it.key]; dup {
				p.rep.Skipped++
				p.diag(Diagnosis{
					Severity: SeverityError, Reason: ReasonDuplicate, Source: d.source, Document: d.index,
					Object:  it.key.String(),
					Message: "already declared in " + first,
					Hint:    "declare each object once; inline objects win over files, then files in name order",
				})
				continue
			}
			p.declared[it.key] = d.source
			items = append(items, it)
		}
	}

	for _, it := range items {
		if ctx.Err() != nil {
			p.diag(Diagnosis{Severity: SeverityError, Reason: ReasonApplyFailed, Source: it.doc.source, Document: it.doc.index, Object: it.key.String(), Message: ctx.Err().Error()})
			p.rep.Skipped++
			continue
		}
		p.apply(ctx, it)
	}
	if r.fileMode && ctx.Err() == nil {
		p.deleteOrphans(ctx)
	}
	p.sortDiagnoses()
	p.rep.Duration = time.Since(p.rep.Started)
	return p.rep
}

// sortDiagnoses orders the diagnoses by source (apply order) and document;
// findings not tied to a current source (directory, store) come first.
func (p *pass) sortDiagnoses() {
	rank := make(map[string]int, len(p.rep.Sources))
	for i, s := range p.rep.Sources {
		rank[s] = i
	}
	r := func(d Diagnosis) int {
		if i, ok := rank[d.Source]; ok {
			return i
		}
		return -1
	}
	sort.SliceStable(p.rep.Diagnoses, func(i, j int) bool {
		a, b := p.rep.Diagnoses[i], p.rep.Diagnoses[j]
		if ra, rb := r(a), r(b); ra != rb {
			return ra < rb
		}
		return a.Document < b.Document
	})
}

func (p *pass) diag(d Diagnosis) { p.rep.Diagnoses = append(p.rep.Diagnoses, d) }

// identify derives the store key of a document without validating it.
func (p *pass) identify(d document) (item, bool) {
	u := &unstructured.Unstructured{Object: d.obj}
	apiVersion, kind, name := u.GetAPIVersion(), u.GetKind(), metaName(d.obj)
	if apiVersion == "" || kind == "" || name == "" {
		p.diag(Diagnosis{
			Severity: SeverityError, Reason: ReasonNotAnObject, Source: d.source, Document: d.index,
			Message: "apiVersion, kind and metadata.name are required",
			Hint:    "objects are CRD-shaped: apiVersion: kudurru.io/v1alpha1, kind, metadata.name, spec",
		})
		return item{}, false
	}
	gv, err := schema.ParseGroupVersion(apiVersion)
	served, namespaced, known := p.r.v.GroupVersionKind(store.Kind(kind))
	if err != nil || !known || gv.Group != served.Group {
		p.diag(Diagnosis{
			Severity: SeverityError, Reason: ReasonUnknownKind, Source: d.source, Document: d.index,
			Object:  kind + "/" + name,
			Message: fmt.Sprintf("%s %s is not a kudurru.io kind", apiVersion, kind),
			Hint:    "known kinds: " + kindList(p.r.v),
		})
		return item{}, false
	}
	ns := metaNamespace(d.obj)
	if namespaced && ns == "" {
		ns = p.r.defaultNS
	}
	return item{doc: d, key: store.Key{Kind: store.Kind(kind), Namespace: ns, Name: name}, gvk: served}, true
}

// apply validates one item and writes it unless it is unchanged or owned
// by another writer.
func (p *pass) apply(ctx context.Context, it item) {
	d := it.doc
	base := Diagnosis{Source: d.source, Document: d.index, Object: it.key.String()}
	skip := func(sev Severity, reason Reason, owner, msg, hint string, fields []string) {
		p.rep.Skipped++
		x := base
		x.Severity, x.Reason, x.Owner, x.Message, x.Hint, x.Fields = sev, reason, owner, msg, hint, fields
		p.diag(x)
	}
	// note is the ownership change recorded once the write succeeds.
	var note *Diagnosis
	noteOnSuccess := func(reason Reason, owner, msg string) {
		x := base
		x.Severity, x.Reason, x.Owner, x.Message = SeverityInfo, reason, owner, msg
		note = &x
	}

	existing, err := newObject(it.gvk)
	if err != nil {
		skip(SeverityError, ReasonUnknownKind, "", err.Error(), "", nil)
		return
	}
	if err := p.r.st.Get(ctx, it.key, existing); errors.Is(err, store.ErrNotFound) {
		existing = nil
	} else if err != nil {
		skip(SeverityError, ReasonApplyFailed, "", "reading the stored object: "+err.Error(), "", nil)
		return
	}

	manager := p.r.manager(d.source)
	force := false
	if existing != nil {
		owner := store.ManagedBy(existing)
		switch {
		case owner == manager:
		case owner == "":
			noteOnSuccess(ReasonAdopted, owner, "adopted an object that had no manager")
		case owner == store.ManagerBootstrap:
			force = true
			noteOnSuccess(ReasonTakenOver, owner, "replaced the first-boot default with the declaration in "+d.source)
		case p.r.fileMode && isFileManager(owner):
			from := strings.TrimPrefix(owner, filePrefix)
			if p.sourceBroken(from) {
				skip(SeverityWarning, ReasonOwnerUnreadable, owner,
					"owned by "+from+", which could not be read completely this pass",
					"fix "+from+"; ownership is decided once it parses again", nil)
				return
			}
			force = true
			noteOnSuccess(ReasonMoved, owner, "moved from "+from)
		default:
			skip(SeverityError, ReasonManagedByOther, owner,
				fmt.Sprintf("exists and is managed by %q; the declaration in %s is ignored", owner, d.source),
				conflictHint(owner, d.source), nil)
			return
		}
	}

	u := &unstructured.Unstructured{Object: runtime.DeepCopyJSON(d.obj)}
	sanitize(u)
	if it.key.Namespace != "" && u.GetNamespace() == "" {
		u.SetNamespace(it.key.Namespace)
	}
	var old *unstructured.Unstructured
	if existing != nil {
		m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(existing)
		if err != nil {
			skip(SeverityError, ReasonApplyFailed, "", "encoding the stored object: "+err.Error(), "", nil)
			return
		}
		old = &unstructured.Unstructured{Object: m}
	}
	if err := p.r.v.ValidateUnstructured(ctx, u, old); err != nil {
		reason, msg, hint, fields := ReasonInvalid, err.Error(), "", []string(nil)
		if ve, ok := validator.AsError(err); ok {
			hint, fields = ve.Hint(), ve.Fields()
		} else if errors.Is(err, validator.ErrUnknownKind) {
			reason = ReasonUnknownKind
		}
		skip(SeverityError, reason, "", msg, hint, fields)
		return
	}
	obj, err := newObject(it.gvk)
	if err == nil {
		err = runtime.DefaultUnstructuredConverter.FromUnstructuredWithValidation(u.Object, obj, true)
	}
	if err != nil {
		skip(SeverityError, ReasonInvalid, "", err.Error(), "", nil)
		return
	}
	obj.GetObjectKind().SetGroupVersionKind(it.gvk)

	if existing != nil && !force && store.ManagedBy(existing) == manager && sameContent(existing, obj, manager) {
		p.rep.Unchanged++
		return
	}
	if _, err := p.r.st.Apply(ctx, obj, store.ApplyOptions{Manager: manager, Force: force}); err != nil {
		var mb *store.ErrManagedBy
		switch {
		case errors.As(err, &mb):
			skip(SeverityError, ReasonManagedByOther, mb.Owner, err.Error(), conflictHint(mb.Owner, d.source), nil)
		case validator.IsInvalid(err):
			var fields []string
			hint := ""
			if ve, ok := validator.AsError(err); ok {
				hint, fields = ve.Hint(), ve.Fields()
			}
			skip(SeverityError, ReasonInvalid, "", err.Error(), hint, fields)
		default:
			skip(SeverityError, ReasonApplyFailed, "", err.Error(), "", nil)
		}
		return
	}
	if note != nil {
		p.diag(*note)
	}
	if existing == nil {
		p.rep.Created++
		p.r.log.Info("objects: created", "object", it.key.String(), "source", d.source)
	} else {
		p.rep.Updated++
		p.r.log.Info("objects: updated", "object", it.key.String(), "source", d.source)
	}
}

// sourceBroken reports whether objects owned by src must be left alone.
func (p *pass) sourceBroken(src string) bool {
	if p.brokenSources[src] {
		return true
	}
	for _, pre := range p.brokenPrefixes {
		if strings.HasPrefix(src, pre) {
			return true
		}
	}
	return false
}

// deleteOrphans deletes file-managed objects that no source declares.
func (p *pass) deleteOrphans(ctx context.Context) {
	for _, kind := range p.r.v.Kinds() {
		opts := store.ListOptions{Limit: listPageSize}
		for {
			page, err := p.r.st.List(ctx, kind, opts)
			if err != nil {
				p.diag(Diagnosis{
					Severity: SeverityWarning, Reason: ReasonListFailed, Source: string(kind),
					Message: "listing " + string(kind) + " failed; orphaned file-managed objects are kept: " + err.Error(),
				})
				break
			}
			for _, obj := range page.Items {
				p.deleteIfOrphan(ctx, kind, obj)
			}
			if page.Continue == "" {
				break
			}
			opts.Continue = page.Continue
		}
	}
}

func (p *pass) deleteIfOrphan(ctx context.Context, kind store.Kind, obj store.Object) {
	owner := store.ManagedBy(obj)
	if !isFileManager(owner) {
		return
	}
	key := store.Key{Kind: kind, Namespace: obj.GetNamespace(), Name: obj.GetName()}
	if _, ok := p.declared[key]; ok {
		return
	}
	from := strings.TrimPrefix(owner, filePrefix)
	if p.sourceBroken(from) {
		return
	}
	err := p.r.st.Delete(ctx, key, store.DeleteOptions{Manager: owner})
	switch {
	case err == nil:
		p.rep.Deleted++
		p.r.log.Info("objects: deleted", "object", key.String(), "source", from)
	case errors.Is(err, store.ErrNotFound):
	default:
		p.diag(Diagnosis{Severity: SeverityError, Reason: ReasonDeleteFailed, Source: from, Object: key.String(), Owner: owner, Message: err.Error()})
	}
}

// sanitize drops what a file must not decide: status (written by
// reconcilers only), server-populated metadata (Snapshot output carries
// it) and the managedBy annotation, which the store sets from the manager.
func sanitize(u *unstructured.Unstructured) {
	delete(u.Object, "status")
	meta, ok := u.Object["metadata"].(map[string]any)
	if !ok {
		return
	}
	for _, f := range []string{"resourceVersion", "uid", "generation", "creationTimestamp", "managedFields", "selfLink", "deletionTimestamp", "deletionGracePeriodSeconds"} {
		delete(meta, f)
	}
	if ann, ok := meta["annotations"].(map[string]any); ok {
		delete(ann, v1alpha1.ManagedByAnnotation)
		if len(ann) == 0 {
			delete(meta, "annotations")
		}
	}
}

// sameContent reports whether applying desired would change nothing:
// everything but metadata and status is equal, and so are the labels and
// annotations (with managedBy set to manager).
func sameContent(existing, desired store.Object, manager string) bool {
	em, err1 := runtime.DefaultUnstructuredConverter.ToUnstructured(existing)
	dm, err2 := runtime.DefaultUnstructuredConverter.ToUnstructured(desired)
	if err1 != nil || err2 != nil {
		return false
	}
	skip := map[string]bool{"metadata": true, "status": true, "apiVersion": true, "kind": true}
	for k := range union(em, dm) {
		if !skip[k] && !reflect.DeepEqual(em[k], dm[k]) {
			return false
		}
	}
	if !equalStrings(existing.GetLabels(), desired.GetLabels()) {
		return false
	}
	want := map[string]string{v1alpha1.ManagedByAnnotation: manager}
	for k, v := range desired.GetAnnotations() {
		if k != v1alpha1.ManagedByAnnotation {
			want[k] = v
		}
	}
	return equalStrings(existing.GetAnnotations(), want)
}

func union(a, b map[string]any) map[string]struct{} {
	out := make(map[string]struct{}, len(a)+len(b))
	for k := range a {
		out[k] = struct{}{}
	}
	for k := range b {
		out[k] = struct{}{}
	}
	return out
}

func equalStrings(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// newObject returns an empty typed object of gvk.
func newObject(gvk schema.GroupVersionKind) (store.Object, error) {
	ro, err := scheme.New(gvk)
	if err != nil {
		return nil, err
	}
	obj, ok := ro.(store.Object)
	if !ok {
		return nil, fmt.Errorf("objects: %s is not a store object", gvk)
	}
	return obj, nil
}

func metaName(obj map[string]any) string {
	meta, _ := obj["metadata"].(map[string]any)
	s, _ := meta["name"].(string)
	return s
}

func metaNamespace(obj map[string]any) string {
	meta, _ := obj["metadata"].(map[string]any)
	s, _ := meta["namespace"].(string)
	return s
}

func kindList(v *validator.Validator) string {
	kinds := v.Kinds()
	s := make([]string, len(kinds))
	for i, k := range kinds {
		s[i] = string(k)
	}
	return strings.Join(s, ", ")
}

func conflictHint(owner, src string) string {
	switch {
	case owner == store.ManagerAPI:
		return "the object was created through the UI, API or CLI; delete it there to let " + src + " manage it, or remove the document"
	case isFileManager(owner):
		return "the object belongs to " + strings.TrimPrefix(owner, filePrefix) + "; remove one of the declarations"
	default:
		return "the object is managed by " + owner + "; remove the document or rename the object"
	}
}
