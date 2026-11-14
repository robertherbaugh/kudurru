package iammigrate

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/iamlegacy"
	"github.com/robertherbaugh/kudurru/internal/store"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

func (m *Migrator) resource(k key) dynamic.ResourceInterface {
	r := m.dynamic.Resource(publicResources[k.Kind])
	if k.Namespace != "" {
		return r.Namespace(k.Namespace)
	}
	return r
}
func (m *Migrator) getPublic(ctx context.Context, k key) (*unstructured.Unstructured, error) {
	rctx, cancel := m.request(ctx)
	obj, err := m.resource(k).Get(rctx, k.Name, metav1.GetOptions{})
	cancel()
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, problem("api-unavailable", k.Kind, k.Namespace, k.Name, "get-target")
	}
	return obj, nil
}
func publicObject(u *unstructured.Unstructured, k key) (store.Object, error) {
	raw, err := json.Marshal(u.Object)
	if err != nil {
		return nil, problem("invalid-target", k.Kind, k.Namespace, k.Name, "decode-target")
	}
	obj, err := iamlegacy.Convert(k.Kind, raw)
	if err != nil || obj.GetNamespace() != k.Namespace || obj.GetName() != k.Name || obj.GetUID() == "" || obj.GetDeletionTimestamp() != nil || len(obj.GetOwnerReferences()) > 0 || len(obj.GetFinalizers()) > 0 {
		return nil, problem("target-conflict", k.Kind, k.Namespace, k.Name, "decode-target")
	}
	return obj, nil
}
func toUnstructured(obj store.Object) (*unstructured.Unstructured, error) {
	raw, err := json.Marshal(obj)
	if err != nil {
		return nil, problem("invalid-source", "", "", "", "encode")
	}
	var value map[string]any
	if json.Unmarshal(raw, &value) != nil {
		return nil, problem("invalid-source", "", "", "", "encode")
	}
	return &unstructured.Unstructured{Object: value}, nil
}

// Begin starts the approved offline plan and returns only after completion/failure.
func (m *Migrator) Begin(ctx context.Context, p *Plan, ack OfflineAcknowledgement) (Status, error) {
	if !ack.ReadersAndWritersStopped {
		return failed(nil, "", problem("offline-required", "", "", "", "begin"))
	}
	if p == nil || p.namespace != m.opts.Namespace {
		return failed(nil, "", problem("invalid-plan", "", "", "", "begin"))
	}
	c, n, err := m.identities(ctx)
	if err != nil {
		return failed(nil, "", err)
	}
	if c != p.cluster || n != p.namespaceUID {
		return failed(nil, "", problem("wrong-cluster", "", "", "", "begin"))
	}
	// Revalidate this opaque plan against this migrator's capabilities/caps;
	// callers may pass a plan produced by another, more permissive instance.
	validated, err := m.makePlan(c, n, p.sources)
	if err != nil {
		return failed(nil, "", err)
	}
	p = validated
	for _, s := range p.sources {
		k := sourceKey(s)
		if _, public := publicResources[k.Kind]; public {
			target, e := m.getPublic(ctx, k)
			if e != nil {
				return failed(nil, "", e)
			}
			if target != nil {
				return failed(nil, "", problem("target-conflict", k.Kind, k.Namespace, k.Name, "begin"))
			}
		}
	}
	sec, _, err := m.loadJournal(ctx)
	if err != nil {
		return failed(nil, "", err)
	}
	if sec != nil {
		return failed(nil, sec.ResourceVersion, problem("CAS-conflict", "Secret", m.opts.Namespace, JournalName, "begin"))
	}
	j := p.journal(uuid.NewString(), uuid.NewString())
	if err = m.verifySources(ctx, j); err != nil {
		return failed(j, "", err)
	}
	data, _ := json.Marshal(j)
	if len(data) > m.opts.Limits.JournalBytes {
		return failed(j, "", problem("bounds-exceeded", "Secret", m.opts.Namespace, JournalName, "begin"))
	}
	rctx, cancel := m.request(ctx)
	sec, err = m.core.CoreV1().Secrets(m.opts.Namespace).Create(rctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: JournalName, Namespace: m.opts.Namespace}, Type: journalType, Data: map[string][]byte{"journal": data}}, metav1.CreateOptions{})
	cancel()
	if err != nil {
		return failed(j, "", problem("CAS-conflict", "Secret", m.opts.Namespace, JournalName, "begin"))
	}
	return m.run(ctx, sec, j, p)
}

// Resume CAS-claims exactly the inspected journal RV after explicit offline recovery.
func (m *Migrator) Resume(ctx context.Context, expectedJournalRV string, ack OfflineAcknowledgement) (Status, error) {
	if !ack.ReadersAndWritersStopped {
		return failed(nil, "", problem("offline-required", "", "", "", "resume"))
	}
	sec, j, err := m.loadJournal(ctx)
	if err != nil {
		return failed(nil, "", err)
	}
	if sec == nil || expectedJournalRV == "" || sec.ResourceVersion != expectedJournalRV {
		return failed(j, "", problem("CAS-conflict", "Secret", m.opts.Namespace, JournalName, "resume"))
	}
	if j.State == Complete {
		return m.Check(ctx)
	}
	// Missing archives can be recovered from unchanged pinned original Secrets.
	if err = m.verifySources(ctx, j); err != nil {
		return failed(j, sec.ResourceVersion, err)
	}
	sources, err := m.sources(ctx, ignoredPrivate(j))
	if err != nil {
		return failed(j, sec.ResourceVersion, err)
	}
	p, err := m.makePlan(j.Cluster, j.NamespaceUID, sources)
	if err != nil {
		return failed(j, sec.ResourceVersion, err)
	}
	if p.journal(j.ID, j.Owner).Manifest != j.Manifest {
		return failed(j, sec.ResourceVersion, problem("source-changed", "", "", "", "resume"))
	}
	if digest(p.journal(j.ID, j.Owner).Public) != digest(unreceipted(j.Public)) || p.counts != j.Counts || !validPrivatePlan(j, p) {
		return failed(j, sec.ResourceVersion, problem("invalid-journal", "Secret", m.opts.Namespace, JournalName, "resume-plan"))
	}
	j.Owner = uuid.NewString()
	j.State = Running
	sec, err = m.save(ctx, sec, j)
	if err != nil {
		return failed(j, "", err)
	}
	return m.run(ctx, sec, j, p)
}

func (m *Migrator) run(ctx context.Context, sec *corev1.Secret, j *journal, p *Plan) (Status, error) {
	fail := func(err error) (Status, error) {
		var prob *Problem
		if errors.As(err, &prob) && prob.Code == "uncertain-create" {
			j.State = RecoveryRequired
			// Never persist an in-memory receipt after its CAS failed. Check
			// derives recovery-required from the durable intent and target.
		}
		return failed(j, sec.ResourceVersion, err)
	}
	if err := m.archive(ctx, j, p); err != nil {
		return fail(err)
	}
	for i := range j.Public {
		k := j.Public[i].Key
		var desired store.Object
		for _, s := range p.sources {
			if sourceKey(s) == k {
				desired = s.obj
				break
			}
		}
		var err error
		sec, err = m.stagePublic(ctx, sec, j, i, desired)
		if err != nil {
			return fail(err)
		}
	}
	for _, r := range j.Public {
		if !p.admins[r.Key] {
			continue
		}
		owner := accountOwner(r.Key, r.UID)
		state := &internalv1.PrincipalState{TypeMeta: metav1.TypeMeta{APIVersion: publicv1.GroupVersion.String(), Kind: "PrincipalState"}, ObjectMeta: metav1.ObjectMeta{Name: internalv1.PrincipalStateName(owner), Generation: 1}, Spec: internalv1.PrincipalStateSpec{Owner: owner, BuiltinAdmin: true}}
		store.SetManagedBy(state, store.ManagerSystem)
		if state.Validate() != nil {
			return fail(problem("private-conflict", "PrincipalState", "", state.Name, "authority"))
		}
		var err error
		sec, err = m.stagePrivate(ctx, sec, j, state)
		if err != nil {
			return fail(err)
		}
	}
	targets := map[key]types.UID{}
	for _, r := range j.Public {
		targets[r.Key] = r.UID
	}
	records, bound, unbound, err := p.credentials(targets)
	if err != nil {
		return fail(err)
	}
	j.Credentials = records
	j.Counts.BoundTokens = bound
	j.Counts.UnboundTokens = unbound
	if err = m.verifySources(ctx, j); err != nil {
		return fail(err)
	}
	if err = m.verifyTargets(ctx, j, p); err != nil {
		return fail(err)
	}
	if _, err = m.archivedPlan(ctx, j); err != nil {
		return fail(err)
	}
	c, n, err := m.identities(ctx)
	if err != nil {
		return fail(err)
	}
	if c != j.Cluster || n != j.NamespaceUID {
		return fail(problem("wrong-cluster", "", "", "", "complete"))
	}
	j.State = Complete
	j.Completion = completion(j)
	next, err := m.save(ctx, sec, j)
	if err != nil {
		return fail(err)
	}
	return status(j, next.ResourceVersion), nil
}

func failed(j *journal, rv string, err error) (Status, error) {
	s := Status{State: Blocked, JournalResourceVersion: rv}
	if j != nil {
		s = status(j, rv)
		s.RemoteAllowed = false
	}
	var p *Problem
	if errors.As(err, &p) {
		cp := *p
		s.Problem = &cp
	}
	return s, err
}

func (m *Migrator) stagePublic(ctx context.Context, sec *corev1.Secret, j *journal, index int, desired store.Object) (*corev1.Secret, error) {
	r := &j.Public[index]
	k := r.Key
	if !r.Intent {
		r.Intent = true
		next, err := m.save(ctx, sec, j)
		if err != nil {
			return sec, err
		}
		sec = next
	}
	current, err := m.getPublic(ctx, k)
	if err != nil {
		return sec, err
	}
	if r.UID == "" {
		if current != nil {
			return sec, problem("uncertain-create", k.Kind, k.Namespace, k.Name, "create")
		}
		candidate := desired.DeepCopyObject().(store.Object)
		candidate.SetUID("")
		candidate.SetResourceVersion("")
		candidate.SetCreationTimestamp(metav1.Time{})
		candidate.SetGeneration(0)
		candidate.SetManagedFields(nil)
		u, err := toUnstructured(candidate)
		if err != nil {
			return sec, err
		}
		delete(u.Object, "status")
		rctx, cancel := m.request(ctx)
		current, err = m.resource(k).Create(rctx, u, metav1.CreateOptions{FieldManager: "kudurru-iam-migration", FieldValidation: "Strict"})
		cancel()
		if err != nil {
			return sec, problem("uncertain-create", k.Kind, k.Namespace, k.Name, "create")
		}
		if current.GetUID() == "" || len(current.GetUID()) > 512 {
			return sec, problem("uncertain-create", k.Kind, k.Namespace, k.Name, "create")
		}
		r.UID = current.GetUID()
		next, err := m.save(ctx, sec, j)
		if err != nil {
			return sec, problem("uncertain-create", k.Kind, k.Namespace, k.Name, "receipt")
		}
		sec = next
	}
	if current == nil {
		return sec, problem("target-conflict", k.Kind, k.Namespace, k.Name, "receipt")
	}
	obj, err := publicObject(current, k)
	if err != nil {
		return sec, err
	}
	if obj.GetUID() != r.UID || publicDigest(obj) != r.Digest {
		return sec, problem("target-conflict", k.Kind, k.Namespace, k.Name, "receipt")
	}
	if !r.StatusDone {
		wanted := statusOf(desired)
		actual := statusOf(obj)
		if digest(actual) != digest(wanted) {
			if !emptyStatus(actual) {
				return sec, problem("target-conflict", k.Kind, k.Namespace, k.Name, "status")
			}
			value, err := toUnstructured(desired)
			if err != nil {
				return sec, err
			}
			current.Object["status"] = value.Object["status"]
			rctx, cancel := m.request(ctx)
			current, err = m.resource(k).UpdateStatus(rctx, current, metav1.UpdateOptions{FieldManager: "kudurru-iam-migration", FieldValidation: "Strict"})
			cancel()
			if err != nil {
				return sec, problem("target-conflict", k.Kind, k.Namespace, k.Name, "status")
			}
			obj, err = publicObject(current, k)
			if err != nil {
				return sec, err
			}
			if obj.GetUID() != r.UID || publicDigest(obj) != r.Digest || digest(statusOf(obj)) != digest(wanted) {
				return sec, problem("target-conflict", k.Kind, k.Namespace, k.Name, "status")
			}
		}
		r.StatusDone = true
		next, err := m.save(ctx, sec, j)
		if err != nil {
			return sec, err
		}
		sec = next
	}
	return sec, nil
}

func statusOf(obj store.Object) any {
	raw, _ := json.Marshal(obj)
	var doc map[string]json.RawMessage
	_ = json.Unmarshal(raw, &doc)
	s := doc["status"]
	if len(s) == 0 || string(s) == "null" {
		return json.RawMessage(`{}`)
	}
	return s
}
func emptyStatus(v any) bool { return digest(v) == digest(json.RawMessage(`{}`)) }

func (m *Migrator) stagePrivate(ctx context.Context, sec *corev1.Secret, j *journal, state *internalv1.PrincipalState) (*corev1.Secret, error) {
	k := key{Kind: "PrincipalState", Name: state.Name}
	i := -1
	for index, r := range j.Private {
		if r.Key == k {
			i = index
			break
		}
	}
	if i < 0 {
		j.Private = append(j.Private, receipt{Key: k, Digest: digest(state), Intent: true})
		i = len(j.Private) - 1
		next, err := m.save(ctx, sec, j)
		if err != nil {
			return sec, err
		}
		sec = next
	}
	r := &j.Private[i]
	name := secretName(k.Kind, "", k.Name)
	rctx, cancel := m.request(ctx)
	target, err := m.core.CoreV1().Secrets(m.opts.Namespace).Get(rctx, name, metav1.GetOptions{})
	cancel()
	if err != nil && !apierrors.IsNotFound(err) {
		return sec, problem("api-unavailable", k.Kind, "", k.Name, "authority")
	}
	if r.UID == "" {
		if err == nil {
			return sec, problem("uncertain-create", k.Kind, "", k.Name, "authority")
		}
		raw, _ := json.Marshal(state)
		candidate := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: m.opts.Namespace, Labels: map[string]string{kindLabel: k.Kind, "app.kubernetes.io/managed-by": "kudurru"}, Annotations: map[string]string{nameAnnotation: k.Name}}, Type: internalType, Data: map[string][]byte{"object": raw}}
		rctx, cancel = m.request(ctx)
		target, err = m.core.CoreV1().Secrets(m.opts.Namespace).Create(rctx, candidate, metav1.CreateOptions{})
		cancel()
		if err != nil || target.UID == "" || len(target.UID) > 512 {
			return sec, problem("uncertain-create", k.Kind, "", k.Name, "authority")
		}
		r.UID = target.UID
		r.StatusDone = true
		next, err := m.save(ctx, sec, j)
		if err != nil {
			return sec, problem("uncertain-create", k.Kind, "", k.Name, "authority-receipt")
		}
		sec = next
	}
	if target == nil || target.UID != r.UID || target.Type != internalType || digest(json.RawMessage(target.Data["object"])) != r.Digest || r.Digest != digest(state) || target.DeletionTimestamp != nil {
		return sec, problem("private-conflict", k.Kind, "", k.Name, "authority")
	}
	return sec, nil
}

func ignoredPrivate(j *journal) map[string]bool {
	result := map[string]bool{}
	for _, r := range j.Private {
		result[secretName(r.Key.Kind, "", r.Key.Name)] = true
	}
	return result
}
func (m *Migrator) verifySources(ctx context.Context, j *journal) error {
	sources, err := m.sources(ctx, ignoredPrivate(j))
	if err != nil {
		return err
	}
	if len(sources) != len(j.Sources) {
		return problem("source-changed", "Secret", m.opts.Namespace, "", "verify")
	}
	for i, s := range sources {
		r := j.Sources[i]
		if sourceKey(s) != r.Key || s.secret.Name != r.Secret || s.secret.UID != r.UID || s.secret.ResourceVersion != r.RV || sourceDigest(s.secret) != r.Digest {
			return problem("source-changed", r.Key.Kind, r.Key.Namespace, r.Key.Name, "verify")
		}
	}
	return nil
}

func (m *Migrator) verifyTargets(ctx context.Context, j *journal, p *Plan) error {
	for _, r := range j.Public {
		u, err := m.getPublic(ctx, r.Key)
		if err != nil {
			return err
		}
		if u == nil {
			return problem("target-conflict", r.Key.Kind, r.Key.Namespace, r.Key.Name, "final")
		}
		obj, err := publicObject(u, r.Key)
		if err != nil {
			return err
		}
		if obj.GetUID() != r.UID || publicDigest(obj) != r.Digest || !r.StatusDone {
			return problem("target-conflict", r.Key.Kind, r.Key.Namespace, r.Key.Name, "final")
		}
		for _, s := range p.sources {
			if sourceKey(s) == r.Key && digest(statusOf(obj)) != digest(statusOf(s.obj)) {
				return problem("target-conflict", r.Key.Kind, r.Key.Namespace, r.Key.Name, "final-status")
			}
		}
	}
	for _, r := range j.Private {
		rctx, cancel := m.request(ctx)
		target, err := m.core.CoreV1().Secrets(m.opts.Namespace).Get(rctx, secretName(r.Key.Kind, "", r.Key.Name), metav1.GetOptions{})
		cancel()
		if err != nil || target.UID != r.UID || target.Type != internalType || digest(json.RawMessage(target.Data["object"])) != r.Digest || target.DeletionTimestamp != nil {
			return problem("private-conflict", r.Key.Kind, "", r.Key.Name, "final")
		}
	}
	return nil
}

// An incomplete journal must not introduce authority outside its pinned source
// plan. Only a receipted public incarnation can have a private target intent.
func validPrivatePlan(j *journal, p *Plan) bool {
	expected := map[key]string{}
	for _, r := range j.Public {
		if r.UID == "" || !p.admins[r.Key] {
			continue
		}
		owner := accountOwner(r.Key, r.UID)
		ps := &internalv1.PrincipalState{TypeMeta: metav1.TypeMeta{APIVersion: publicv1.GroupVersion.String(), Kind: "PrincipalState"}, ObjectMeta: metav1.ObjectMeta{Name: internalv1.PrincipalStateName(owner), Generation: 1}, Spec: internalv1.PrincipalStateSpec{Owner: owner, BuiltinAdmin: true}}
		store.SetManagedBy(ps, store.ManagerSystem)
		expected[key{Kind: "PrincipalState", Name: ps.Name}] = digest(ps)
	}
	for _, r := range j.Private {
		if !r.Intent || expected[r.Key] != r.Digest {
			return false
		}
	}
	return true
}
