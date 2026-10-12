package objects

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utiljson "k8s.io/apimachinery/pkg/util/json"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

// maxDocumentBytes bounds one YAML document (a DNSRecordSet at the CRD
// size ceiling is well below this).
const maxDocumentBytes = 4 << 20

// document is one decoded YAML document of a source.
type document struct {
	source string
	index  int // 1-based position in source
	obj    map[string]any
}

// decoded is the result of reading one source.
type decoded struct {
	docs  []document
	diags []Diagnosis
	// broken is set when part of the source could not be understood (a
	// YAML error), so the objects it owns must not be deleted this pass.
	broken bool
}

// decodeStream splits a multi-document YAML (or JSON) stream into
// documents. A syntax error in one document is reported and the remaining
// documents are still decoded; duplicate mapping keys are errors (strict
// YAML). Documents of kind List are flattened into their items, so
// Store.Snapshot output and `kubectl get -o yaml` lists load verbatim.
func decodeStream(source string, data []byte) decoded {
	var out decoded
	r := utilyaml.NewYAMLReader(bufio.NewReaderSize(bytes.NewReader(data), 64<<10))
	index := 0
	for {
		chunk, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			out.broken = true
			out.diags = append(out.diags, Diagnosis{
				Severity: SeverityError, Reason: ReasonInvalidYAML, Source: source, Document: index + 1,
				Message: "cannot split the YAML stream: " + err.Error(),
				Hint:    "objects already applied from this source are kept until it parses again",
			})
			break
		}
		if len(chunk) > maxDocumentBytes {
			index++
			out.broken = true
			out.diags = append(out.diags, Diagnosis{
				Severity: SeverityError, Reason: ReasonInvalidYAML, Source: source, Document: index,
				Message: fmt.Sprintf("document is larger than %d bytes", maxDocumentBytes),
			})
			continue
		}
		j, err := yaml.YAMLToJSONStrict(chunk)
		if err != nil {
			index++
			out.broken = true
			out.diags = append(out.diags, Diagnosis{
				Severity: SeverityError, Reason: ReasonInvalidYAML, Source: source, Document: index,
				Message: err.Error(),
				Hint:    "objects already applied from this source are kept until it parses again",
			})
			continue
		}
		j = bytes.TrimSpace(j)
		if len(j) == 0 || string(j) == "null" {
			continue // empty document or comments only
		}
		var v any
		if err := utiljson.Unmarshal(j, &v); err != nil {
			index++
			out.broken = true
			out.diags = append(out.diags, Diagnosis{
				Severity: SeverityError, Reason: ReasonInvalidYAML, Source: source, Document: index,
				Message: err.Error(),
			})
			continue
		}
		out.add(source, &index, v)
	}
	return out
}

// decodeInline converts the inline objects of the config file (decoded by
// koanf, so Go ints and nested maps) to JSON-shaped documents.
func decodeInline(source string, items []map[string]any) decoded {
	var out decoded
	index := 0
	for _, item := range items {
		b, err := json.Marshal(item)
		var v any
		if err == nil {
			err = utiljson.Unmarshal(b, &v)
		}
		if err != nil {
			index++
			out.broken = true
			out.diags = append(out.diags, Diagnosis{
				Severity: SeverityError, Reason: ReasonInvalidYAML, Source: source, Document: index,
				Message: "cannot encode inline object: " + err.Error(),
			})
			continue
		}
		out.add(source, &index, v)
	}
	return out
}

// add appends v as one document, or its items when v is a kind: List.
func (d *decoded) add(source string, index *int, v any) {
	m, ok := v.(map[string]any)
	if !ok {
		*index++
		d.broken = true
		d.diags = append(d.diags, Diagnosis{
			Severity: SeverityError, Reason: ReasonNotAnObject, Source: source, Document: *index,
			Message: fmt.Sprintf("document is a %T, not a mapping", v),
			Hint:    "each document must be one object with apiVersion, kind, metadata and spec",
		})
		return
	}
	u := &unstructured.Unstructured{Object: m}
	if u.IsList() {
		items, _ := m["items"].([]any)
		for _, it := range items {
			d.add(source, index, it)
		}
		return
	}
	*index++
	d.docs = append(d.docs, document{source: source, index: *index, obj: m})
}
