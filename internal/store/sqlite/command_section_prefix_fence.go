package sqlite

import (
	"context"
	"strings"

	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

// sectionPrefixFence is one bounded private registration, not grant authority.
// Only actual later section mutations may set changed, and it never resets.
// The command is synchronous; final checks read this memory after callback exit.
type sectionPrefixFence struct {
	section string
	prefix  string
	changed bool
}

// RequireSectionPrefixUnchanged preserves an already validated section-prefix
// postimage in this SAME Command. Register after the complete intended owner is
// validated and staged, including when that owner's operation is unchanged.
// Prefix uses the existing literal UTF-8 byte-prefix rules; %, _ and backslash
// have no wildcard meaning. This grants no writer, source or proof authority.
//
// Every later actual PutSection/DeleteSection change under the original prefix
// rejects the WHOLE command, including another row or ABA. An identical Put or
// absent Delete may pass. Re-registering cannot replace an earlier fence. The
// existing MaxCommitChecks budget bounds private markers and final checks.
// The final closure reads only its private monotonic marker, with no tx/Store
// calls, parsing, I/O or locks. Original writer/context/proof gates still apply.
func (t *CommandTx) RequireSectionPrefixUnchanged(ctx context.Context, section, prefix string) error {
	if t == nil {
		return ErrTransactionEnded
	}
	if !t.active.Load() {
		return t.poison(ErrTransactionEnded)
	}
	if ctx == nil {
		return t.poison(replication.ErrInvalid)
	}
	if err := t.valid(ctx); err != nil {
		return err
	}
	if !validSectionPrefix(section, prefix) {
		return t.poison(replication.ErrInvalid)
	}
	marker := &sectionPrefixFence{section: section, prefix: prefix}
	if err := t.RequireCommitCheck(func(context.Context) error {
		if marker.changed {
			return replication.ErrCAS
		}
		return nil
	}); err != nil {
		return err
	}
	t.sectionFences = append(t.sectionFences, marker)
	return nil
}

// markSectionMutation is called ONLY after SQL and RowsAffected succeed with a
// positive actual change. Tracking one monotonic bit per registration preserves
// ABA without an unbounded per-write journal. Private registrations cannot be
// removed or cleared by later operations. Never call this for reads/no-ops.
func (t *CommandTx) markSectionMutation(section, key string) {
	for _, marker := range t.sectionFences {
		if marker.section == section && strings.HasPrefix(key, marker.prefix) {
			marker.changed = true
		}
	}
}
