package objects

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store"
)

func inlineZone(ttl int) string {
	return fmt.Sprintf(`node: {name: unrelated}
objects:
  - apiVersion: kudurru.io/v1alpha1
    kind: DNSZone
    metadata: {name: home}
    spec: {zoneName: home.example., type: primary, defaultTTL: %d}
`, ttl)
}

func writeInline(t *testing.T, name, body string) {
	t.Helper()
	tmp := filepath.Join(filepath.Dir(name), ".inline-replacement")
	if err := os.WriteFile(tmp, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, name); err != nil {
		t.Fatal(err)
	}
}

func TestInlineFileRetainsWholeSourceAndOwnership(t *testing.T) {
	name := filepath.Join(t.TempDir(), "node.yaml")
	f := newFixture(t, nil, func(o *Options) { o.InlineFile = name })
	f.write(t, "other.yaml", listsYAML)
	writeInline(t, name, inlineZone(100))
	if rep := f.l.Load(ctx(t)); rep.Count(SeverityError) != 0 {
		t.Fatalf("initial load: %+v", rep)
	}
	rv := zone(t, f.st, "home").ResourceVersion
	for label, body := range map[string]string{
		"syntax":           "auth: { password: do-not-report-this [\n",
		"duplicate":        inlineZone(200) + "objects: []\n",
		"not mapping":      "[]\n",
		"empty":            "",
		"bad List items":   "objects: [{apiVersion: v1, kind: List, items: wrong}]\n",
		"wrong list":       "objects: {secret: do-not-report-this}\n",
		"bad item":         inlineZone(200) + "  - not-an-object\n",
		"bad schema":       inlineZone(200) + "  - {apiVersion: kudurru.io/v1alpha1, kind: DNSZone, metadata: {name: invalid}, spec: {zoneName: invalid., type: unknown}}\n",
		"two docs":         inlineZone(200) + "---\nobjects: []\n",
		"duplicate object": inlineZone(200) + "  - {apiVersion: kudurru.io/v1alpha1, kind: DNSZone, metadata: {name: home}, spec: {zoneName: home.example., type: primary}}\n",
	} {
		t.Run(label, func(t *testing.T) {
			writeInline(t, name, body)
			rep := f.l.Load(ctx(t))
			if rep.Count(SeverityError) == 0 || rep.Updated != 0 || rep.Deleted != 0 {
				t.Fatalf("broken source partially applied: %+v", rep)
			}
			if strings.Contains(fmt.Sprint(rep.Diagnoses), "do-not-report-this") {
				t.Fatal("inline diagnosis exposed configuration content")
			}
			if z := zone(t, f.st, "home"); z == nil || z.ResourceVersion != rv {
				t.Fatal("last good inline object changed")
			}
		})
	}
	if err := os.Remove(name); err != nil {
		t.Fatal(err)
	}
	// An unreadable owner cannot be stolen by a newly added directory source.
	f.write(t, "home.yaml", "apiVersion: kudurru.io/v1alpha1\nkind: DNSZone\nmetadata: {name: home}\nspec: {zoneName: home.example., type: primary, defaultTTL: 900}\n")
	f.l.Load(ctx(t))
	if z := zone(t, f.st, "home"); z == nil || z.ResourceVersion != rv {
		t.Fatal("unreadable inline owner was removed or taken over")
	}
	f.remove(t, "home.yaml")
	writeInline(t, name, inlineZone(300))
	if rep := f.l.Load(ctx(t)); rep.Count(SeverityError) != 0 || rep.Updated != 1 {
		t.Fatalf("recovery: %+v", rep)
	}
	writeInline(t, name, "objects: []\n")
	if rep := f.l.Load(ctx(t)); rep.Deleted != 1 || rep.Count(SeverityError) != 0 {
		t.Fatalf("explicit removal: %+v", rep)
	}
	if zone(t, f.st, "home") != nil || owner(t, f.st, nsKey("BlockList", "default", "ads")) != store.ManagerFile("objects.d/other.yaml") {
		t.Fatal("inline removal affected unrelated directory objects")
	}
}

func TestInlineFileWatchAtomicReplaceAndProjection(t *testing.T) {
	for _, projected := range []bool{false, true} {
		t.Run(fmt.Sprintf("projected=%v", projected), func(t *testing.T) {
			root := t.TempDir()
			name := filepath.Join(root, "node.yaml")
			put := func(version string, ttl int) {
				if !projected {
					writeInline(t, name, inlineZone(ttl))
					return
				}
				dir := filepath.Join(root, ".."+version)
				if err := os.Mkdir(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				writeInline(t, filepath.Join(dir, "node.yaml"), inlineZone(ttl))
				if err := os.Symlink(".."+version, filepath.Join(root, "..data-tmp")); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(filepath.Join(root, "..data-tmp"), filepath.Join(root, "..data")); err != nil {
					t.Fatal(err)
				}
			}
			put("one", 100)
			if projected {
				if err := os.Symlink("..data/node.yaml", name); err != nil {
					t.Fatal(err)
				}
			}
			f := newFixture(t, nil, func(o *Options) { o.InlineFile, o.Dir, o.PollInterval = name, "", -1 })
			runLoader(t, f.l)
			eventually(t, "inline source watched", func() bool { return f.l.Status().Watching && zone(t, f.st, "home") != nil })
			put("two", 200)
			eventually(t, "inline edit published without polling", func() bool { z := zone(t, f.st, "home"); return z != nil && z.Spec.DefaultTTL == 200 })
			put("three", 300)
			eventually(t, "watch survives replacement", func() bool { z := zone(t, f.st, "home"); return z != nil && z.Spec.DefaultTTL == 300 })
		})
	}
}

func TestInlineFilePollAndSizeBound(t *testing.T) {
	root := t.TempDir()
	name := filepath.Join(root, "late", "node.yaml")
	f := newFixture(t, nil, func(o *Options) { o.InlineFile, o.Dir, o.PollInterval = name, "", 50*time.Millisecond })
	runLoader(t, f.l)
	eventually(t, "missing source diagnosed", func() bool { return f.l.Status().Errors > 0 })
	if err := os.Mkdir(filepath.Dir(name), 0o700); err != nil {
		t.Fatal(err)
	}
	writeInline(t, name, inlineZone(100))
	eventually(t, "late file recovered", func() bool { return zone(t, f.st, "home") != nil && f.l.Status().Watching })
	file, err := os.CreateTemp(filepath.Dir(name), ".oversized-inline-*")
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(maxFileBytes + 1); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(file.Name(), name); err != nil {
		t.Fatal(err)
	}
	f.l.Load(ctx(t))
	if z := zone(t, f.st, "home"); z == nil || z.Spec.DefaultTTL != 100 || f.l.Status().Errors == 0 {
		t.Fatal("oversized source did not retain last good object")
	}
}
