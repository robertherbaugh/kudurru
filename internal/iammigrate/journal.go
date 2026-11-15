package iammigrate

import (
	"context"
	"encoding/json"
	"regexp"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var identityPattern = regexp.MustCompile(`^[a-zA-Z0-9-]{36}$`)
var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (m *Migrator) loadJournal(ctx context.Context) (*corev1.Secret, *journal, error) {
	rctx, cancel := m.request(ctx)
	sec, err := m.core.CoreV1().Secrets(m.opts.Namespace).Get(rctx, JournalName, metav1.GetOptions{})
	cancel()
	if apierrors.IsNotFound(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, problem("api-unavailable", "Secret", m.opts.Namespace, JournalName, "read")
	}
	var j journal
	if sec.Type != journalType || len(sec.Data) != 1 || sec.DeletionTimestamp != nil || sec.UID == "" || !strict(sec.Data["journal"], &j, m.opts.Limits.JournalBytes) {
		return sec, nil, problem("invalid-journal", "Secret", m.opts.Namespace, JournalName, "decode")
	}
	if j.Version != journalVersion || j.Converter != 1 {
		return sec, nil, problem("incompatible-version", "Secret", m.opts.Namespace, JournalName, "decode")
	}
	if j.Namespace != m.opts.Namespace || !identityPattern.MatchString(j.ID) || !identityPattern.MatchString(j.Owner) || j.Manifest != digest(j.Sources) || len(j.Sources) > m.opts.Limits.Objects || len(j.Public) > len(j.Sources) || len(j.Private) > len(j.Public) || len(j.Credentials) > len(j.Sources) || j.Cluster == "" || j.NamespaceUID == "" {
		return sec, nil, problem("invalid-journal", "Secret", m.opts.Namespace, JournalName, "validate")
	}
	if j.State != Running && j.State != RecoveryRequired && j.State != Complete {
		return sec, nil, problem("invalid-journal", "Secret", m.opts.Namespace, JournalName, "state")
	}
	seen := map[key]bool{}
	publicSources := map[key]bool{}
	for _, s := range j.Sources {
		if !sourceKinds[s.Key.Kind] || seen[s.Key] || s.Secret != secretName(s.Key.Kind, s.Key.Namespace, s.Key.Name) || s.UID == "" || len(s.UID) > 512 || s.RV == "" || !digestPattern.MatchString(s.Digest) || s.Archive != "kudurru-iam-source-"+digest([]string{j.ID, s.Secret})[:48] {
			return sec, nil, problem("invalid-journal", "Secret", m.opts.Namespace, JournalName, "source")
		}
		seen[s.Key] = true
		if _, public := publicResources[s.Key.Kind]; public {
			publicSources[s.Key] = true
		}
	}
	if len(publicSources) != len(j.Public) {
		return sec, nil, problem("invalid-journal", "Secret", m.opts.Namespace, JournalName, "receipt")
	}
	seen = map[key]bool{}
	for _, r := range j.Public {
		if _, ok := publicResources[r.Key.Kind]; !ok || !publicSources[r.Key] || seen[r.Key] || !digestPattern.MatchString(r.Digest) || len(r.UID) > 512 || (!r.Intent && (r.UID != "" || r.StatusDone)) || (r.StatusDone && r.UID == "") {
			return sec, nil, problem("invalid-journal", "Secret", m.opts.Namespace, JournalName, "receipt")
		}
		seen[r.Key] = true
	}
	seen = map[key]bool{}
	for _, r := range j.Private {
		if r.Key.Kind != "PrincipalState" || r.Key.Namespace != "" || seen[r.Key] || len(r.UID) > 512 || !digestPattern.MatchString(r.Digest) || (!r.Intent && r.UID != "") || (r.StatusDone && r.UID == "") {
			return sec, nil, problem("invalid-journal", "Secret", m.opts.Namespace, JournalName, "authority")
		}
		seen[r.Key] = true
	}
	if j.State == Complete && (j.Completion != completion(&j) || !allReceipted(&j)) {
		return sec, nil, problem("invalid-journal", "Secret", m.opts.Namespace, JournalName, "completion")
	}
	c, n, err := m.identities(ctx)
	if err != nil {
		return sec, nil, err
	}
	if c != j.Cluster || n != j.NamespaceUID {
		return sec, nil, problem("wrong-cluster", "Secret", m.opts.Namespace, JournalName, "identity")
	}
	return sec, &j, nil
}

func allReceipted(j *journal) bool {
	for _, r := range append(append([]receipt(nil), j.Public...), j.Private...) {
		if r.UID == "" || !r.Intent || !r.StatusDone {
			return false
		}
	}
	return true
}
func completion(j *journal) string {
	return digest(struct {
		Manifest        string
		Public, Private []receipt
		Credentials     []credentialRecord
		Counts          Counts
	}{j.Manifest, j.Public, j.Private, j.Credentials, j.Counts})
}

func (m *Migrator) save(ctx context.Context, sec *corev1.Secret, j *journal) (*corev1.Secret, error) {
	data, err := json.Marshal(j)
	if err != nil || len(data) > m.opts.Limits.JournalBytes {
		return nil, problem("bounds-exceeded", "Secret", m.opts.Namespace, JournalName, "CAS")
	}
	// The caller holds the last read UID/RV. A stale owner can never use a
	// freshly loaded RV without matching its run owner in the runner.
	next := sec.DeepCopy()
	next.Data = map[string][]byte{"journal": data}
	rctx, cancel := m.request(ctx)
	updated, err := m.core.CoreV1().Secrets(m.opts.Namespace).Update(rctx, next, metav1.UpdateOptions{})
	cancel()
	if apierrors.IsConflict(err) {
		return nil, problem("CAS-conflict", "Secret", m.opts.Namespace, JournalName, "CAS")
	}
	if err != nil {
		return nil, problem("api-unavailable", "Secret", m.opts.Namespace, JournalName, "CAS")
	}
	return updated, nil
}
