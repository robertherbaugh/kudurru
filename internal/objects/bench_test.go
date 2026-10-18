package objects

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/store/memory"
)

// BenchmarkLoadUnchanged measures the steady-state cost of one poll: 500
// objects in 10 files, all already applied (validate + Get + compare, plus
// the orphan scan).
func BenchmarkLoadUnchanged(b *testing.B) {
	dir := filepath.Join(b.TempDir(), "objects.d")
	if err := os.Mkdir(dir, 0o750); err != nil {
		b.Fatal(err)
	}
	for f := range 10 {
		var sb strings.Builder
		for i := range 50 {
			fmt.Fprintf(&sb, "---\napiVersion: kudurru.io/v1alpha1\nkind: DNSZone\nmetadata: { name: z%d-%d }\nspec: { zoneName: z%d-%d.example., type: primary }\n", f, i, f, i)
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%02d.yaml", f)), []byte(sb.String()), 0o600); err != nil {
			b.Fatal(err)
		}
	}
	st := memory.New()
	defer func() { _ = st.Close() }()
	l, err := New(Options{Store: st, Validator: testValidator(b), Dir: dir, Logger: quiet})
	if err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	if rep := l.Load(ctx); rep.Created != 500 {
		b.Fatalf("initial load: %+v", rep)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if rep := l.Load(ctx); rep.Unchanged != 500 {
			b.Fatalf("load: %+v", rep)
		}
	}
}
