package objects

import (
	"fmt"
	"strings"
	"time"
)

// Severity grades a Diagnosis.
type Severity string

// Severities. An error means a document was not applied (the previously
// stored object, if any, is kept); a warning means a source was skipped or
// only partly understood; info records an ownership change.
const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
	SeverityInfo    Severity = "info"
)

// Reason is the stable machine-readable key of a Diagnosis. The UI and CLI
// switch on it; the message is for humans.
type Reason string

// Reasons.
const (
	// ReasonReadFailed: a source file or the objects directory could not be
	// read. Objects it owned are kept.
	ReasonReadFailed Reason = "ReadFailed"
	// ReasonInvalidYAML: a document is not valid YAML (or has duplicate
	// keys). Objects owned by the source are kept.
	ReasonInvalidYAML Reason = "InvalidYAML"
	// ReasonNotAnObject: a document is valid YAML but not a kudurru object
	// (no apiVersion/kind/metadata.name, or not a mapping).
	ReasonNotAnObject Reason = "NotAnObject"
	// ReasonUnknownKind: apiVersion/kind do not name a kudurru.io kind.
	ReasonUnknownKind Reason = "UnknownKind"
	// ReasonInvalid: the object fails CRD validation (schema, CEL, strict
	// unknown fields). Fields lists the offending JSON paths.
	ReasonInvalid Reason = "Invalid"
	// ReasonDuplicate: the same object is declared more than once; the first
	// declaration (inline objects, then files in name order) wins.
	ReasonDuplicate Reason = "Duplicate"
	// ReasonManagedByOther: the object exists and is managed by the API, an
	// integration or the system; file declarations never override them.
	ReasonManagedByOther Reason = "ManagedByOther"
	// ReasonOwnerUnreadable: the object belongs to another file that could
	// not be read this pass, so ownership cannot be decided.
	ReasonOwnerUnreadable Reason = "OwnerUnreadable"
	// ReasonApplyFailed and ReasonDeleteFailed: the store rejected a write
	// for a reason other than validation or ownership.
	ReasonApplyFailed  Reason = "ApplyFailed"
	ReasonDeleteFailed Reason = "DeleteFailed"
	// ReasonListFailed: the store could not be listed, so orphaned
	// file-managed objects were not deleted this pass.
	ReasonListFailed Reason = "ListFailed"
	// ReasonAdopted: an object without an owner was adopted by a file.
	ReasonAdopted Reason = "Adopted"
	// ReasonTakenOver: a first-boot seed object (managedBy bootstrap) was
	// replaced by the file declaration.
	ReasonTakenOver Reason = "TakenOver"
	// ReasonMoved: the object moved from one file to another.
	ReasonMoved Reason = "Moved"
)

// Diagnosis is one finding of a load, shaped for the UI and CLI. A load
// never aborts on a bad document: it records a Diagnosis and continues.
type Diagnosis struct {
	Severity Severity `json:"severity"`
	Reason   Reason   `json:"reason"`
	// Source is the manager-relative source name ("objects.d/zones.yaml",
	// "kudurru.yaml") or, for ApplyFile, the path as given.
	Source string `json:"source"`
	// Document is the 1-based position of the document in Source (items of
	// a kind: List count individually); 0 refers to the whole source.
	Document int `json:"document,omitempty"`
	// Object is the store key ("DNSZone/home", "Policy/dns/kids") when the
	// document could be identified.
	Object string `json:"object,omitempty"`
	// Owner is the current metadata.managedBy for ownership findings.
	Owner string `json:"owner,omitempty"`
	// Fields are the offending JSON paths for ReasonInvalid.
	Fields  []string `json:"fields,omitempty"`
	Message string   `json:"message"`
	Hint    string   `json:"hint,omitempty"`
}

// String renders "objects.d/zones.yaml#2 DNSZone/home: message".
func (d Diagnosis) String() string {
	var b strings.Builder
	b.WriteString(d.Source)
	if d.Document > 0 {
		fmt.Fprintf(&b, "#%d", d.Document)
	}
	if d.Object != "" {
		b.WriteString(" ")
		b.WriteString(d.Object)
	}
	b.WriteString(": ")
	b.WriteString(d.Message)
	return b.String()
}

// Report is the outcome of one load or ApplyFile call.
type Report struct {
	// Sources are the sources read, in apply order.
	Sources []string `json:"sources"`
	// Objects counts the documents identified as kudurru objects.
	Objects int `json:"objects"`
	// Created, Updated, Unchanged and Deleted count store outcomes; Skipped
	// counts identified documents that were not applied (see Diagnoses).
	Created   int `json:"created"`
	Updated   int `json:"updated"`
	Unchanged int `json:"unchanged"`
	Deleted   int `json:"deleted"`
	Skipped   int `json:"skipped"`

	Diagnoses []Diagnosis   `json:"diagnoses,omitempty"`
	Started   time.Time     `json:"started"`
	Duration  time.Duration `json:"duration"`
}

// Count returns the number of diagnoses with severity s.
func (r Report) Count(s Severity) int {
	n := 0
	for _, d := range r.Diagnoses {
		if d.Severity == s {
			n++
		}
	}
	return n
}

// Errors returns the error diagnoses.
func (r Report) Errors() []Diagnosis {
	var out []Diagnosis
	for _, d := range r.Diagnoses {
		if d.Severity == SeverityError {
			out = append(out, d)
		}
	}
	return out
}
