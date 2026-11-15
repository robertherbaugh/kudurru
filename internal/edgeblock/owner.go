package edgeblock

import (
	"strings"

	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

// ValidateEntry validates a replicated runtime row of this owner. Rows of
// other owners pass; a row under Prefix must be the one versioned document.
func ValidateEntry(section string, row replication.Entry) error {
	if section != Section || !strings.HasPrefix(row.Key, Prefix) {
		return nil
	}
	if row.Key != Key {
		return ErrState
	}
	_, err := Decode(row.Data)
	return err
}

// ValidateImage refuses an image with more than one owner row or a malformed
// one. Absence is the never-written empty set.
func ValidateImage(image *replication.Image) error {
	if image == nil || !image.Complete {
		return ErrState
	}
	_, err := FromRows(image.Payload.Runtime)
	return err
}

// FromRows returns the state stored in runtime rows (the empty set when the
// owner has never been written).
func FromRows(rows []replication.Entry) (State, error) {
	var found *replication.Entry
	for i := range rows {
		if !strings.HasPrefix(rows[i].Key, Prefix) {
			continue
		}
		if found != nil || rows[i].Key != Key {
			return State{}, ErrState
		}
		found = &rows[i]
	}
	if found == nil {
		return Empty(), nil
	}
	return Decode(found.Data)
}
