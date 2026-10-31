package auth

// ExternalProof binds an external principal to immutable owner evidence. The
// owner must reopen and validate it before trusting principal fields. SessionID
// selects a captured durable enrollment; Evidence carries authenticated request
// facts. Neither is an accepted HTTP credential or command authorization proof.
type ExternalProof struct {
	ProviderUID string
	Origin      string
	SessionID   string
	Evidence    string
}

func (ExternalProof) String() string { return "private external identity evidence" }

// GoString prevents diagnostic disclosure through nested principal formatting.
func (p ExternalProof) GoString() string { return p.String() }

// MarshalJSON prevents accidental persistence or response disclosure.
func (ExternalProof) MarshalJSON() ([]byte, error) { return []byte(`{"private":true}`), nil }
