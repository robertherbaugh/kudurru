package listfetcher

import (
	"io"
	"os"
	"strings"

	"github.com/robertherbaugh/kudurru/internal/blocking"
)

// openedSources holds the files behind one list's compile Spec. Each body
// is opened once and read through section readers, so a newer body renamed
// over the cache file mid-compile cannot make the checksum pass and the
// parse pass see different bytes.
type openedSources struct {
	files []*os.File
}

func (o *openedSources) close() {
	for _, f := range o.files {
		_ = f.Close()
	}
	o.files = nil
}

// fileSource returns a blocking.Source over an already open cache file.
func (o *openedSources) fileSource(name string, ps *plannedSource, f *os.File, size int64) blocking.Source {
	o.files = append(o.files, f)
	return blocking.Source{Name: name, Format: ps.format, HostsSubdomains: ps.subdomains, Open: func() (io.ReadCloser, error) {
		return io.NopCloser(io.NewSectionReader(f, 0, size)), nil
	}}
}

// fingerprint builder for "did the inputs of this list change".
type fpBuilder struct{ b strings.Builder }

func (f *fpBuilder) add(parts ...string) {
	for _, p := range parts {
		f.b.WriteString(p)
		f.b.WriteByte(0)
	}
	f.b.WriteByte('\n')
}

func (f *fpBuilder) String() string { return f.b.String() }
