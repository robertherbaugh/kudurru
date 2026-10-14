package audit

import (
	"encoding/json"

	internalkinds "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
)

// RedactDocuments returns an event safe for audit readers and exports, including
// historical rows written before credential fields were excluded. Stored hashes
// describe the original documents; verification must use the unredacted store.
func RedactDocuments(ev Event) Event {
	if !internalkinds.IsInternal(ev.Resource.Kind) {
		return ev
	}
	ev.Before = redactDocument(ev.Resource.Kind, ev.Before)
	ev.After = redactDocument(ev.Resource.Kind, ev.After)
	return ev
}

func redactDocument(kind string, doc json.RawMessage) json.RawMessage {
	if kind != internalkinds.KindAPIToken || len(doc) == 0 {
		return nil
	}
	var tok internalkinds.APIToken
	if err := json.Unmarshal(doc, &tok); err != nil {
		return nil
	}
	return tok.AuditDocument()
}
