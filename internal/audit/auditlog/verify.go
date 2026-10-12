package auditlog

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"sigs.k8s.io/controller-runtime/pkg/client"

	auditv1 "github.com/robertherbaugh/kudurru/api/audit/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/audit"
)

// Verify checks every batch in the namespace: each event's hash (stubs
// only for their link), the links between events and between the batches
// of one source, and, when keys are given (the seal keys of the current
// and previous encryption keys), each batch's seal. It names the first
// batch that fails. The oldest remaining batch of a source may follow
// pruned ones; a batch removed from the middle shows as a broken link,
// one removed from the end of a source does not (the "log":"audit" lines
// are the copy to compare against).
func Verify(ctx context.Context, c client.Reader, namespace string, keys [][]byte) error {
	var list auditv1.AuditLogList
	if err := c.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return fmt.Errorf("auditlog: list batches: %w", err)
	}
	bySource := map[string][]*auditv1.AuditLog{}
	for i := range list.Items {
		b := &list.Items[i]
		bySource[b.Spec.Source] = append(bySource[b.Spec.Source], b)
	}
	sealed := false
	for _, k := range keys {
		sealed = sealed || len(k) > 0
	}
	var errs []error
	for _, batches := range bySource {
		sort.Slice(batches, func(i, j int) bool { return batches[i].Spec.FirstID < batches[j].Spec.FirstID })
		for i, b := range batches {
			if err := verifyBatch(b, sealed, keys); err != nil {
				errs = append(errs, err)
				continue
			}
			if i > 0 && b.Spec.PrevHash != batches[i-1].Spec.Hash {
				errs = append(errs, fmt.Errorf("auditlog: batch %s does not follow batch %s (a batch between them is missing or changed)", b.Name, batches[i-1].Name))
			}
		}
	}
	return errors.Join(errs...)
}

func verifyBatch(b *auditv1.AuditLog, sealed bool, keys [][]byte) error {
	s := &b.Spec
	if sealed && !sealOK(keys, *s) {
		return fmt.Errorf("auditlog: batch %s: the seal does not verify (changed, or written without the cluster encryption key)", b.Name)
	}
	if len(s.Events) == 0 || len(s.Events) != s.Count {
		return fmt.Errorf("auditlog: batch %s: %d events, spec.count %d", b.Name, len(s.Events), s.Count)
	}
	first, last := s.Events[0], s.Events[len(s.Events)-1]
	if first.PrevHash != s.PrevHash || last.Hash != s.Hash || first.ID != s.FirstID || last.ID != s.LastID {
		return fmt.Errorf("auditlog: batch %s: events do not match the batch's ids and hashes", b.Name)
	}
	var prev string
	for i := range s.Events {
		e := &s.Events[i]
		if i > 0 && e.PrevHash != prev {
			return fmt.Errorf("auditlog: batch %s: event %d does not follow event %d", b.Name, e.ID, s.Events[i-1].ID)
		}
		if !e.Stub {
			if want := audit.ChainHash(toEvent(s, e), e.PrevHash); want != e.Hash {
				return fmt.Errorf("auditlog: batch %s: event %d was changed", b.Name, e.ID)
			}
		}
		prev = e.Hash
	}
	return nil
}
