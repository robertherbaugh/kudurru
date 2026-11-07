package objects

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utiljson "k8s.io/apimachinery/pkg/util/json"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"

	"github.com/robertherbaugh/kudurru/internal/store"
)

// readInlineFile captures one bounded file. Decode and validate the entire
// objects section before reconciliation so a damaged source cannot remove or
// partially replace its last good declarations. Store writes still use the
// normal per-object ownership, validation and writer-fencing checks.
func (l *Loader) readInlineFile(ctx context.Context) source {
	s := source{name: l.opts.InlineSource}
	fail := func(reason Reason, document int, message string) source {
		s.decoded = decoded{broken: true, diags: []Diagnosis{{
			Severity: SeverityError, Reason: reason, Source: s.name,
			Document: document, Message: message,
			Hint: "the entire inline source is retained until it can be read and validated",
		}}}
		return s
	}
	b, skip, err := readFile(l.opts.InlineFile)
	if err != nil || skip {
		return fail(ReasonReadFailed, 0, "cannot read a regular inline configuration file within the size limit")
	}
	// YAMLToJSONStrict reads one document. Reject a trailing document explicitly
	// rather than silently reconciling just the first part of the node file.
	r := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(b)))
	chunk, err := r.Read()
	if err != nil {
		return fail(ReasonInvalidYAML, 0, "inline configuration must contain one YAML mapping")
	}
	if _, err := r.Read(); !errors.Is(err, io.EOF) {
		return fail(ReasonInvalidYAML, 0, "inline configuration must contain exactly one YAML document")
	}
	j, err := yaml.YAMLToJSONStrict(chunk)
	if err != nil {
		// Parser errors can include unrelated credentials from the node file.
		return fail(ReasonInvalidYAML, 0, "inline configuration has invalid YAML or duplicate keys")
	}
	var root map[string]any
	if err := utiljson.Unmarshal(j, &root); err != nil || root == nil {
		return fail(ReasonInvalidYAML, 0, "inline configuration must be a mapping")
	}
	value := root["objects"]
	if value == nil {
		return s // an explicit empty/null/removed section removes owned objects
	}
	items, ok := value.([]any)
	if !ok {
		return fail(ReasonInvalidYAML, 0, "objects must be a list of object mappings")
	}
	index := 0
	for _, v := range items {
		s.add(s.name, &index, v)
	}
	if s.broken {
		return fail(ReasonNotAnObject, 0, "every inline item must be an object mapping")
	}
	seen := map[store.Key]bool{}
	p := &pass{r: l.rec}
	for _, d := range s.docs {
		if ctx.Err() != nil {
			return fail(ReasonReadFailed, d.index, "inline validation canceled")
		}
		encoded, err := json.Marshal(d.obj)
		if err != nil || len(encoded) > maxDocumentBytes {
			return fail(ReasonInvalid, d.index, "inline object exceeds the document size limit")
		}
		it, ok := p.identify(d)
		if !ok {
			return fail(ReasonNotAnObject, d.index, "inline object must name a known kind, API version and object name")
		}
		if seen[it.key] {
			return fail(ReasonDuplicate, d.index, "inline objects must have distinct kind, namespace and name")
		}
		seen[it.key] = true
		u := &unstructured.Unstructured{Object: runtime.DeepCopyJSON(d.obj)}
		sanitize(u)
		if it.key.Namespace != "" && u.GetNamespace() == "" {
			u.SetNamespace(it.key.Namespace)
		}
		if err := l.rec.v.ValidateUnstructured(ctx, u, nil); err != nil {
			return fail(ReasonInvalid, d.index, "inline object fails schema validation")
		}
	}
	return s
}
