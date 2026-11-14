package audit

import (
	"encoding/json"

	"github.com/robertherbaugh/kudurru/internal/auditdocument"
)

// RedactDocuments protects both new recordings and historical reader/export
// documents. Verification always uses original stored bytes, never this copy.
func RedactDocuments(ev Event) Event {
	ev.Before = auditdocument.Redact(ev.Resource.Kind, ev.Before)
	ev.After = auditdocument.Redact(ev.Resource.Kind, ev.After)
	return ev
}

// PrepareDocuments redacts before capping and hashing a new recording. Historical
// verifier canonicalization must never call this function.
func PrepareDocuments(ev Event) Event {
	ev.Before = Document(ev.Resource.Kind, ev.Before)
	ev.After = Document(ev.Resource.Kind, ev.After)
	return ev
}

// Document redacts an original document before applying the audit size cap.
// Use the runtime kind when known so private carriers are omitted as well.
func Document(kind string, value any) json.RawMessage {
	if value == nil {
		return nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	safe := auditdocument.Redact(kind, data)
	if len(safe) > MaxDiff {
		safe, _ = json.Marshal(map[string]any{"truncated": true, "bytes": len(safe)})
	}
	return safe
}
