package zone

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

// rebuild turns an import back into a zone the way the controller would:
// Objects, then Build with the imported serial stamped into status.
func rebuild(t testing.TB, im *Imported) (*Zone, Errors) {
	t.Helper()
	dz, sets := im.Objects("imported", "dns", "imported")
	dz.Status.Serial = im.Serial
	return Build(dz, sets, BuildOptions{Now: testNow})
}

func export(t testing.TB, z *Zone) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := ExportBIND(&buf, z); err != nil {
		t.Fatalf("export: %v", err)
	}
	return buf.Bytes()
}

func TestBINDRoundTrip(t *testing.T) {
	for name, z := range map[string]*Zone{
		"example": exampleZone(t),
		"root": mustBuild(t, dnsZone(".", func(dz *v1alpha1.DNSZone) { dz.Spec.Nameservers = []string{"a.root.test"} }),
			ep("test", "NS", "a.root.test"), ep("a.root.test", "A", "192.0.2.1"), ep("@", "TXT", "root apex")),
		"escapes": mustBuild(t, dnsZone("example.org."),
			ep(`a\.b.example.org`, "A", "192.0.2.1"),
			ep(`sp\032ace.example.org`, "TXT", `tab	and "quote" and \backslash`, "caf\xc3\xa9"),
			ep("long.example.org", "TXT", strings.Repeat("0123456789", 40)),
		),
	} {
		t.Run(name, func(t *testing.T) {
			first := export(t, z)
			im, err := ImportBIND(bytes.NewReader(first), z.Name())
			if err != nil {
				t.Fatalf("import of our own export: %v\n%s", err, first)
			}
			if len(im.Warnings) != 0 {
				t.Fatalf("lossless export must import without warnings: %q", im.Warnings)
			}
			if im.Serial != z.Serial() {
				t.Fatalf("serial %d, want %d", im.Serial, z.Serial())
			}
			z2, errs := rebuild(t, im)
			if !errs.Empty() {
				t.Fatalf("rebuild: %v %v", errs.Zone, errs.RecordSets)
			}
			if z2.ContentHash() != z.ContentHash() || z2.RecordCount() != z.RecordCount() {
				t.Fatalf("content changed: %s/%d vs %s/%d\n%s", z2.ContentHash(), z2.RecordCount(), z.ContentHash(), z.RecordCount(), first)
			}
			if second := export(t, z2); !bytes.Equal(first, second) {
				t.Fatalf("export is not a fixed point:\n--- first\n%s\n--- second\n%s", first, second)
			}
		})
	}
}

const bindFixture = `; a hand-written zone in the usual BIND style
$ORIGIN example.com.
$TTL 3600
@	IN	SOA	ns1 hostmaster (
		2024010101 ; serial
		7200 1800 1209600 600 )
	IN	NS	ns1
	IN	NS	ns2.example.net.
	IN	MX	10 mail
	300 IN	A	192.0.2.1
ns1	IN	A	192.0.2.53
mail	IN	A	192.0.2.25
www	60	IN	CNAME	@
txt	IN	TXT	"v=spf1 mx -all"
multi	IN	A	192.0.2.10
multi	120	IN	A	192.0.2.11
*.apps	IN	A	192.0.2.80
sub	IN	NS	ns.sub
ns.sub	IN	A	192.0.2.54
weird	IN	TYPE65280 \# 2 beef
info	IN	HINFO	"cpu" "os"
@	IN	DNSKEY	256 3 13 AAAA
$GENERATE 1-3 host$ A 192.0.2.$
`

func TestImportBIND(t *testing.T) {
	im, err := ImportBIND(strings.NewReader(bindFixture), "Example.COM")
	if err != nil {
		t.Fatal(err)
	}
	spec := im.Spec
	if spec.ZoneName != "example.com." || spec.Type != v1alpha1.ZoneTypePrimary || spec.DefaultTTL != 3600 || im.Serial != 2024010101 {
		t.Fatalf("spec: %+v serial %d", spec, im.Serial)
	}
	if *spec.SOA != (v1alpha1.SOASpec{MName: "ns1.example.com.", RName: "hostmaster.example.com.", Refresh: 7200, Retry: 1800, Expire: 1209600, Minimum: 600}) {
		t.Fatalf("SOA: %+v", spec.SOA)
	}
	if strings.Join(spec.Nameservers, " ") != "ns1.example.com. ns2.example.net." {
		t.Fatalf("nameservers: %v", spec.Nameservers)
	}
	got := map[string]string{}
	for _, e := range im.Endpoints {
		got[e.DNSName+"/"+e.RecordType] = fmt.Sprintf("%d %s", e.RecordTTL, strings.Join(e.Targets, "|"))
	}
	for k, want := range map[string]string{
		"example.com/MX":              "0 10 mail.example.com.",
		"example.com/A":               "300 192.0.2.1",
		"www.example.com/CNAME":       "60 example.com.",
		"txt.example.com/TXT":         `0 "v=spf1 mx -all"`,
		"multi.example.com/A":         "120 192.0.2.10|192.0.2.11",
		"*.apps.example.com/A":        "0 192.0.2.80",
		"sub.example.com/NS":          "0 ns.sub.example.com.",
		"weird.example.com/TYPE65280": `0 \# 2 beef`,
		"info.example.com/TYPE13":     `0 \# 7 03637075026f73`,
		"host2.example.com/A":         "0 192.0.2.2",
	} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}
	if _, ok := got["example.com/DNSKEY"]; ok {
		t.Error("DNSSEC records are skipped")
	}
	warn := strings.Join(im.Warnings, "\n")
	for _, w := range []string{"DNSKEY", "multi.example.com. A: records with different TTLs"} {
		if !strings.Contains(warn, w) {
			t.Errorf("warnings %q lack %q", im.Warnings, w)
		}
	}
	z, errs := rebuild(t, im)
	if !errs.Empty() {
		t.Fatalf("rebuild: %v %v", errs.Zone, errs.RecordSets)
	}
	if r := z.Lookup("x.apps.example.com.", 1); r.Kind != Success || !r.Wildcard {
		t.Fatalf("wildcard after import: %+v", r)
	}
	if r := z.Lookup("info.example.com.", 13); r.Kind != Success || r.Answer[0].String() != `info.example.com.	3600	IN	HINFO	"cpu" "os"` {
		t.Fatalf("RFC 3597 import serves the original record: %v", r.Answer)
	}
}

func TestImportBINDErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		file, want string
	}{
		"outside":   {"other.org. 300 IN A 192.0.2.1\n", "outside zone"},
		"two-soa":   {"@ IN SOA a. b. 1 2 3 4 5\n@ IN SOA a. b. 2 2 3 4 5\n", "more than one SOA"},
		"soa-below": {"x IN SOA a. b. 1 2 3 4 5\n", "not at the apex"},
		"class":     {"x CH TXT hi\n", "class CH"},
		"include":   {"$INCLUDE /etc/passwd\n", "include"},
		"syntax":    {"x IN A not-an-address\n", "import"},
		"meta-type": {`x IN TYPE41 \# 0` + "\n", "cannot be served"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ImportBIND(strings.NewReader(tc.file), "example.com.")
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tc.want)) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
	importLimit = 100
	defer func() { importLimit = MaxImportRecords }()
	if _, err := ImportBIND(strings.NewReader("$GENERATE 0-100 h$ A 192.0.2.1\n"), "example.com."); err == nil || !strings.Contains(err.Error(), "more than 100 records") {
		t.Fatalf("record cap: %v", err)
	}
	if _, err := ImportBIND(strings.NewReader("$GENERATE 1-100 h$ A 192.0.2.1\n"), "example.com."); err != nil {
		t.Fatalf("at the cap: %v", err)
	}
	if _, err := ImportBIND(strings.NewReader(""), "a..b"); err == nil {
		t.Fatal("bad origin")
	}
	im, err := ImportBIND(strings.NewReader("x IN A 192.0.2.1\n"), "example.com.")
	if err != nil || len(im.Warnings) != 1 || !strings.Contains(im.Warnings[0], "no SOA") || im.Spec.DefaultTTL != DefaultTTL {
		t.Fatalf("missing SOA: %v %+v", err, im)
	}
}

func TestImportedObjectsSplit(t *testing.T) {
	im := &Imported{Spec: v1alpha1.DNSZoneSpec{ZoneName: "example.com.", Type: v1alpha1.ZoneTypePrimary}}
	for i := range 2*MaxEndpointsPerRecordSet + 1 {
		im.Endpoints = append(im.Endpoints, ep(fmt.Sprintf("h%d.example.com", i), "A", "192.0.2.1"))
	}
	dz, sets := im.Objects("example-com", "dns", "import")
	if dz.Name != "example-com" || dz.Kind != "DNSZone" || len(sets) != 3 {
		t.Fatalf("objects: %v %d", dz.Name, len(sets))
	}
	for i, want := range []struct {
		name string
		n    int
	}{{"import", 1000}, {"import-2", 1000}, {"import-3", 1}} {
		s := sets[i]
		if s.Name != want.name || s.Namespace != "dns" || len(s.Spec.Endpoints) != want.n || s.Spec.ZoneRef.Name != "example-com" {
			t.Errorf("set %d: %s/%s %d", i, s.Namespace, s.Name, len(s.Spec.Endpoints))
		}
	}
}

func TestExportForwardZone(t *testing.T) {
	z, _ := Build(dnsZone("corp.example.", func(dz *v1alpha1.DNSZone) { dz.Spec.Type = v1alpha1.ZoneTypeForward }), nil, BuildOptions{})
	if err := ExportBIND(&bytes.Buffer{}, z); err == nil {
		t.Fatal("forward zones have nothing to export")
	}
	if err := ExportBIND(failWriter{}, exampleZone(t)); err == nil {
		t.Fatal("write errors are reported")
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

// FuzzImportBIND feeds arbitrary master files to the importer. Whatever
// imports must build without panicking, and whatever builds cleanly must
// survive export and re-import with the same content hash and a
// byte-identical second export.
func FuzzImportBIND(f *testing.F) {
	f.Add(bindFixture)
	f.Add("$TTL 60\n@ SOA a b 1 2 3 4 5\n@ NS ns\nns A 192.0.2.1\n")
	f.Add("x 300 IN TXT \"a\\\"b\" c\\032d\n")
	f.Add(`a\.b IN CNAME c` + "\n")
	f.Add("* IN MX 0 .\n")
	f.Add("d IN DNAME example.net.\nx.d IN A 192.0.2.1\n")
	f.Add("s IN HTTPS 1 . alpn=h2 ipv4hint=192.0.2.1\n")
	f.Add("c IN CAA 0 issue \"ca.example\"\n")
	f.Add("n IN NAPTR 100 10 \"u\" \"E2U+sip\" \"!^.*$!sip:x@example.com!\" .\n")
	f.Add("$GENERATE 1-2 h$ A 192.0.2.$\n")
	f.Fuzz(func(t *testing.T, file string) {
		im, err := ImportBIND(strings.NewReader(file), "example.com.")
		if err != nil {
			return
		}
		z, errs := rebuild(t, im)
		if z == nil || !errs.Empty() {
			return
		}
		first := export(t, z)
		im2, err := ImportBIND(bytes.NewReader(first), "example.com.")
		if err != nil {
			t.Fatalf("our export does not import: %v\n%s", err, first)
		}
		z2, errs := rebuild(t, im2)
		if !errs.Empty() {
			t.Fatalf("our export does not rebuild: %v %v\n%s", errs.Zone, errs.RecordSets, first)
		}
		if z2.ContentHash() != z.ContentHash() {
			t.Fatalf("round trip changed content:\n%s\n---\n%s", first, export(t, z2))
		}
		if second := export(t, z2); !bytes.Equal(first, second) {
			t.Fatalf("export is not a fixed point:\n%s\n---\n%s", first, second)
		}
	})
}
