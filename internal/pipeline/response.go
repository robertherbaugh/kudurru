package pipeline

import "time"

// ValidationState describes the checked raw answer and its dependencies.
// Modified responses do not authenticate newly synthesized records.
type ValidationState uint8

const (
	ValidationUnknown ValidationState = iota
	ValidationSecure
	ValidationInsecure
	ValidationBogus
	ValidationIndeterminate
)

func (s ValidationState) String() string {
	if int(s) < len(validationNames) {
		return validationNames[s]
	}
	return "unknown"
}

var validationNames = [...]string{"unknown", "secure", "insecure", "bogus", "indeterminate"}

type ResponseOrigin uint8

const (
	OriginUnknown ResponseOrigin = iota
	OriginForwarded
	OriginAuthoritative
	OriginLocal
	OriginSynthetic
	OriginMixed
	OriginFailure
)

// Security is immutable validation provenance, never inferred from wire AD.
type Security struct {
	State       ValidationState
	Reason      string
	ContextID   [32]byte
	ValidatedAt time.Time
	ValidUntil  time.Time
}

// AvailableAt uses a strict deadline, including for stale data.
func (s Security) AvailableAt(now time.Time) bool {
	return (s.ValidatedAt.IsZero() || !now.Before(s.ValidatedAt)) &&
		(s.ValidUntil.IsZero() || now.Before(s.ValidUntil))
}

// CacheScope identifies raw resolution semantics and fences replacement work.
type CacheScope struct {
	ID    [32]byte
	Epoch uint64
}
