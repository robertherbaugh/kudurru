package clientid

import "testing"

func TestClientIDBoundaries(t *testing.T) {
	for _, input := range []string{"", "-x", "x-", "x.y", "x/y", "x%2fy", " x", "é", "a\x00"} {
		if _, ok := Normalize(input); ok {
			t.Errorf("accepted %q", input)
		}
	}
	if got := FromSNI("PHONE.dot.Example.com.", "dot.example.com"); got != "phone" {
		t.Fatal(got)
	}
	for _, sni := range []string{"phone.notdot.example.com", "x.phone.dot.example.com", "dot.example.com", "phone.dot.example.com.evil"} {
		if got := FromSNI(sni, "dot.example.com"); got != "" {
			t.Fatalf("accepted SNI %q as %q", sni, got)
		}
	}
	if id, ok := FromPath("/dns-query/Phone", "/dns-query", true); !ok || id != "phone" {
		t.Fatal(id, ok)
	}
	for _, path := range []string{"/dns-query/", "/dns-query//phone", "/dns-query/phone/x", "/dns-queryevil/phone"} {
		if _, ok := FromPath(path, "/dns-query", true); ok {
			t.Fatalf("accepted path %q", path)
		}
	}
	if _, ok := FromPath("/dns-query/phone", "/dns-query", false); ok {
		t.Fatal("accepted disabled client ID")
	}
}
