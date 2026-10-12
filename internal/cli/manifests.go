package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

// Manifest is one object document read from a file.
type Manifest struct {
	// Source is the file name ("-" for stdin) and Index the document's
	// position in it (0-based), for error messages.
	Source string
	Index  int
	// Object is the document as JSON.
	Object json.RawMessage
	Meta   Meta
}

// Ref formats "kind/name" or "kind/namespace/name" for messages.
func (m Manifest) Ref() string {
	return ObjectRef(m.Meta.Kind, m.Meta.Metadata.Namespace, m.Meta.Metadata.Name)
}

// ObjectRef formats an object reference the way apply and delete print it:
// lower-case kind, then the namespace when there is one, then the name.
func ObjectRef(kind, namespace, name string) string {
	k := strings.ToLower(kind)
	if namespace != "" {
		return k + "/" + namespace + "/" + name
	}
	return k + "/" + name
}

// ReadManifests reads every document from the given paths: files,
// directories (their *.yaml, *.yml and *.json files, recursively when
// recursive is set, in lexical order) and "-" for stdin. Documents may be
// multi-document YAML, a single JSON object, a JSON array, or a List with
// items. Empty documents are skipped.
func ReadManifests(paths []string, stdin io.Reader, recursive bool) ([]Manifest, error) {
	if len(paths) == 0 {
		return nil, errors.New("no manifests given (use -f file|dir|-)")
	}
	var out []Manifest
	for _, p := range paths {
		if p == "-" {
			ms, err := ParseManifests("-", stdin)
			if err != nil {
				return nil, err
			}
			out = append(out, ms...)
			continue
		}
		files, err := manifestFiles(p, recursive)
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			fh, err := os.Open(f) //nolint:gosec // the user names the manifests to apply
			if err != nil {
				return nil, err
			}
			ms, err := ParseManifests(f, fh)
			_ = fh.Close()
			if err != nil {
				return nil, err
			}
			out = append(out, ms...)
		}
	}
	return out, nil
}

func manifestFiles(p string, recursive bool) ([]string, error) {
	st, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return []string{p}, nil
	}
	var files []string
	err = filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != p && (!recursive || strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		switch strings.ToLower(filepath.Ext(path)) {
		case ".yaml", ".yml", ".json":
			files = append(files, path)
		}
		return nil
	})
	sort.Strings(files)
	return files, err
}

// ParseManifests splits one stream into documents.
func ParseManifests(source string, r io.Reader) ([]Manifest, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", source, err)
	}
	trimmed := bytes.TrimSpace(data)
	var raws []json.RawMessage
	if len(trimmed) > 0 && trimmed[0] == '[' {
		if err := json.Unmarshal(trimmed, &raws); err != nil {
			return nil, fmt.Errorf("%s: %w", source, err)
		}
	} else {
		yr := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
		for {
			doc, err := yr.Read()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("%s: %w", source, err)
			}
			if len(bytes.TrimSpace(stripComments(doc))) == 0 {
				continue
			}
			j, err := yaml.YAMLToJSON(doc)
			if err != nil {
				return nil, fmt.Errorf("%s (document %d): %w", source, len(raws), err)
			}
			if string(j) == "null" {
				continue
			}
			raws = append(raws, j)
		}
	}
	var out []Manifest
	for _, raw := range raws {
		items, err := expandList(raw)
		if err != nil {
			return nil, fmt.Errorf("%s (document %d): %w", source, len(out), err)
		}
		for _, it := range items {
			m := Manifest{Source: source, Index: len(out), Object: it}
			if m.Meta, err = DecodeMeta(it); err != nil {
				return nil, fmt.Errorf("%s (document %d): %w", source, m.Index, err)
			}
			if m.Meta.Kind == "" {
				return nil, fmt.Errorf("%s (document %d): no kind", source, m.Index)
			}
			if m.Meta.Metadata.Name == "" {
				return nil, fmt.Errorf("%s (document %d): %s has no metadata.name", source, m.Index, m.Meta.Kind)
			}
			out = append(out, m)
		}
	}
	return out, nil
}

// expandList returns the items of a "kind: List" (or *List) document, or
// the document itself.
func expandList(raw json.RawMessage) ([]json.RawMessage, error) {
	var probe struct {
		Kind  string            `json:"kind"`
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, err
	}
	if probe.Kind == "List" || (strings.HasSuffix(probe.Kind, "List") && probe.Items != nil) {
		return probe.Items, nil
	}
	return []json.RawMessage{raw}, nil
}

func stripComments(doc []byte) []byte {
	var b bytes.Buffer
	for _, line := range bytes.Split(doc, []byte("\n")) {
		if t := bytes.TrimSpace(line); len(t) == 0 || t[0] == '#' {
			continue
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	return b.Bytes()
}
