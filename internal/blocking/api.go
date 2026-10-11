package blocking

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/blocking/parser"
)

// ErrModeMismatch is returned by SelectPolicy when a policy references a
// list under blockListRefs that is an allow list, or the reverse.
var ErrModeMismatch = errors.New("blocking: list mode does not match the policy reference")

// SourceOpener opens a URL or ConfigMap source of a BlockList. The list
// fetcher supplies it and serves the cached body (lists/<sha256(url)>, or
// the body pulled from the leader by digest).
type SourceOpener func(index int, src v1alpha1.ListSource) (func() (io.ReadCloser, error), error)

// SpecFromAPI maps a BlockList spec onto a compile Spec named name (the
// name shown in the query log and metrics). Inline sources are read from
// the object; URL and ConfigMap sources are opened through open. An empty
// mode is the CRD default, block.
func SpecFromAPI(name string, spec *v1alpha1.BlockListSpec, open SourceOpener) (Spec, error) {
	mode := Mode(spec.Mode)
	if mode == "" {
		mode = ModeBlock
	}
	out := Spec{Name: name, Mode: mode}
	for i, src := range spec.Sources {
		sname := sourceName(i, src)
		format := parser.Format(src.Format)
		if len(src.Inline) > 0 {
			out.Sources = append(out.Sources, LinesSource(sname, format, src.Inline))
			continue
		}
		if open == nil {
			return Spec{}, fmt.Errorf("blocking: list %q source %q needs a fetcher", name, sname)
		}
		fn, err := open(i, src)
		if err != nil {
			return Spec{}, fmt.Errorf("blocking: list %q source %q: %w", name, sname, err)
		}
		out.Sources = append(out.Sources, Source{Name: sname, Format: format, Open: fn})
	}
	return out, nil
}

func sourceName(i int, src v1alpha1.ListSource) string {
	switch {
	case src.Name != "":
		return src.Name
	case src.URL != "":
		return src.URL
	case src.ConfigMapRef != nil:
		return "configmap/" + src.ConfigMapRef.Name
	}
	return fmt.Sprintf("inline-%d", i)
}

// SelectPolicy resolves a Policy's list references. Each list keeps the
// role its own mode gives it; a reference under the wrong field is reported
// with ErrModeMismatch so the controller can surface it as a condition, but
// the list is still applied by its mode. Unknown lists are reported with
// ErrUnknownList as in Select.
func (s *Snapshot) SelectPolicy(p *v1alpha1.PolicySpec, scope ExceptionScope) (*Selection, error) {
	refs := make([]string, 0, len(p.AllowListRefs)+len(p.BlockListRefs))
	refs = append(refs, p.AllowListRefs...)
	refs = append(refs, p.BlockListRefs...)
	sel, err := s.Select(refs, SelectOptions{AllowOnly: p.AllowOnly, ExceptionScope: scope})
	var wrong []string
	for _, n := range p.BlockListRefs {
		if l := s.byName[n]; l != nil && l.mode != ModeBlock {
			wrong = append(wrong, n)
		}
	}
	for _, n := range p.AllowListRefs {
		if l := s.byName[n]; l != nil && l.mode != ModeAllow {
			wrong = append(wrong, n)
		}
	}
	if len(wrong) > 0 {
		err = errors.Join(err, fmt.Errorf("%w: %s", ErrModeMismatch, strings.Join(wrong, ", ")))
	}
	return sel, err
}
