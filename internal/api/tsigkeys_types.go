package api

import (
	"context"
	"time"
)

// TSIGKeyOperation is one explicit stored credential lifecycle intent.
type TSIGKeyOperation string

// Stored-key lifecycle operations never invoke generic object CRUD.
const (
	TSIGKeyCreate  TSIGKeyOperation = "create"
	TSIGKeyReplace TSIGKeyOperation = "replace"
	TSIGKeyRevoke  TSIGKeyOperation = "revoke"
	TSIGKeyReseal  TSIGKeyOperation = "reseal"
)

// TSIGKeyMetadata contains safe bindings only. File keys are read-only and
// omit durable UID/resourceVersion/creation time; no secret path is disclosed.
type TSIGKeyMetadata struct {
	KeyName         string     `json:"keyName" maxLength:"254"`
	Algorithm       string     `json:"algorithm" maxLength:"64"`
	Source          string     `json:"source" enum:"file,store"`
	UID             string     `json:"uid,omitempty" maxLength:"256"`
	ResourceVersion string     `json:"resourceVersion,omitempty" maxLength:"256"`
	CreatedAt       *time.Time `json:"createdAt,omitempty"`
}

// TSIGKeyCommandReceipt is emitted only after confirmed actual commit.
// Published describes exact complete publication observed under the original
// operation lifetime, not permanent availability or replica acknowledgment.
// For Removed=true, Key is historical deleted metadata, not a surviving row.
type TSIGKeyCommandReceipt struct {
	Operation TSIGKeyOperation `json:"operation" enum:"create,replace,revoke,reseal"`
	Committed bool             `json:"committed" doc:"Always true in a receipt: actual command completion was confirmed."`
	Published bool             `json:"published" doc:"Exact target publication acknowledged within original bounds; false does not imply rollback or absence of publication."`
	Removed   bool             `json:"removed"`
	Key       TSIGKeyMetadata  `json:"key"`
}

// TSIGKeyList is detached original-publication metadata, never private carriers.
type TSIGKeyList struct {
	Items []TSIGKeyMetadata `json:"items" maxItems:"1024"`
}

// TSIGKeyChange is bounded write-only input. It grants no command authority.
type TSIGKeyChange struct {
	Operation       TSIGKeyOperation
	KeyName         string
	Algorithm       string
	UID             string
	ResourceVersion string
	Secret          []byte
}

func (TSIGKeyChange) String() string { return "private TSIG key change" }

// GoString returns a redacted representation without credential material.
func (c TSIGKeyChange) GoString() string { return c.String() }

// MarshalJSON returns a redacted marker without credential material.
func (TSIGKeyChange) MarshalJSON() ([]byte, error) { return []byte(`{"private":true}`), nil }

// TSIGKeyReadSource reads safe metadata from the original complete request cut.
type TSIGKeyReadSource func(context.Context) ([]TSIGKeyMetadata, error)

// TSIGKeyCommand returns a receipt only after confirmed actual commit. An error
// may include an uncertain commit outcome; callers must not infer rollback or
// replay a mutation. No legacy Store or memory-only writer fallback exists.
type TSIGKeyCommand func(context.Context, TSIGKeyChange) (*TSIGKeyCommandReceipt, error)

// CreateStoredTSIGKeyBody provisions only an absent name; Secret is write-only
// canonical standard base64, at most 512 decoded bytes.
type CreateStoredTSIGKeyBody struct {
	KeyName   string `json:"keyName" minLength:"1" maxLength:"254"`
	Algorithm string `json:"algorithm" minLength:"1" maxLength:"64"`
	Secret    string `json:"secret" minLength:"4" maxLength:"684" writeOnly:"true"`
}

// ReplaceStoredTSIGKeyBody requires reviewed original durable bindings.
type ReplaceStoredTSIGKeyBody struct {
	UID             string `json:"uid" minLength:"1" maxLength:"256"`
	ResourceVersion string `json:"resourceVersion" minLength:"1" maxLength:"256"`
	Algorithm       string `json:"algorithm,omitempty" maxLength:"64"`
	Secret          string `json:"secret" minLength:"4" maxLength:"684" writeOnly:"true"`
}

// StoredTSIGKeyBinding is the entire revoke/reseal input. Neither operation
// accepts a secret, algorithm or arbitrary carrier fields.
type StoredTSIGKeyBinding struct {
	UID             string `json:"uid" minLength:"1" maxLength:"256"`
	ResourceVersion string `json:"resourceVersion" minLength:"1" maxLength:"256"`
}

func (CreateStoredTSIGKeyBody) String() string { return "private TSIG create body" }

// GoString returns a redacted representation without credential material.
func (b CreateStoredTSIGKeyBody) GoString() string { return b.String() }

// MarshalJSON returns a redacted marker without credential material.
func (CreateStoredTSIGKeyBody) MarshalJSON() ([]byte, error) { return []byte(`{"private":true}`), nil }
func (ReplaceStoredTSIGKeyBody) String() string              { return "private TSIG replacement body" }

// GoString returns a redacted representation without credential material.
func (b ReplaceStoredTSIGKeyBody) GoString() string { return b.String() }

// MarshalJSON returns a redacted marker without credential material.
func (ReplaceStoredTSIGKeyBody) MarshalJSON() ([]byte, error) { return []byte(`{"private":true}`), nil }
