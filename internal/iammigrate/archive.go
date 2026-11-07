package iammigrate

import (
	"context"
	"encoding/json"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (m *Migrator) archive(ctx context.Context, j *journal, p *Plan) error {
	for i, s := range p.sources {
		record := j.Sources[i]
		raw, err := json.Marshal(s.secret)
		if err != nil || len(raw) > 768<<10 {
			return problem("bounds-exceeded", record.Key.Kind, record.Key.Namespace, record.Key.Name, "archive")
		}
		immutable := true
		sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: record.Archive, Namespace: m.opts.Namespace}, Type: archiveType, Immutable: &immutable, Data: map[string][]byte{"source": raw}}
		rctx, cancel := m.request(ctx)
		_, err = m.core.CoreV1().Secrets(m.opts.Namespace).Create(rctx, sec, metav1.CreateOptions{})
		cancel()
		if err != nil && !apierrors.IsAlreadyExists(err) {
			return problem("api-unavailable", "Secret", m.opts.Namespace, record.Archive, "archive")
		}
		if _, err = m.readArchive(ctx, record); err != nil {
			return err
		}
	}
	return nil
}

func (m *Migrator) readArchive(ctx context.Context, r sourceRecord) (*source, error) {
	rctx, cancel := m.request(ctx)
	archive, err := m.core.CoreV1().Secrets(m.opts.Namespace).Get(rctx, r.Archive, metav1.GetOptions{})
	cancel()
	if err != nil {
		return nil, problem("missing-archive", r.Key.Kind, r.Key.Namespace, r.Key.Name, "archive")
	}
	var sec corev1.Secret
	if archive.Type != archiveType || archive.Immutable == nil || !*archive.Immutable || archive.DeletionTimestamp != nil || len(archive.Data) != 1 || !strict(archive.Data["source"], &sec, 768<<10) || sec.Name != r.Secret || sec.UID != r.UID || sec.ResourceVersion != r.RV || sourceDigest(&sec) != r.Digest {
		return nil, problem("invalid-archive", r.Key.Kind, r.Key.Namespace, r.Key.Name, "archive")
	}
	s, err := m.decodeSource(&sec)
	if err != nil {
		return nil, err
	}
	if sourceKey(s) != r.Key {
		return nil, problem("invalid-archive", r.Key.Kind, r.Key.Namespace, r.Key.Name, "archive")
	}
	return s, nil
}

func (m *Migrator) archivedPlan(ctx context.Context, j *journal) (*Plan, error) {
	var sources []*source
	total := 0
	for _, r := range j.Sources {
		s, err := m.readArchive(ctx, r)
		if err != nil {
			return nil, err
		}
		sources = append(sources, s)
		total += len(s.secret.Data["object"])
		if total > m.opts.Limits.TotalBytes {
			return nil, problem("bounds-exceeded", "Secret", m.opts.Namespace, "", "archive")
		}
	}
	p, err := m.makePlan(j.Cluster, j.NamespaceUID, sources)
	if err != nil {
		return nil, err
	}
	projected := p.journal(j.ID, j.Owner)
	if projected.Manifest != j.Manifest || digest(projected.Public) != digest(unreceipted(j.Public)) {
		return nil, problem("invalid-journal", "Secret", m.opts.Namespace, JournalName, "plan")
	}
	return p, nil
}

func unreceipted(rs []receipt) []receipt {
	result := append([]receipt(nil), rs...)
	for i := range result {
		result[i].UID = ""
		result[i].Intent = false
		result[i].StatusDone = false
	}
	return result
}
