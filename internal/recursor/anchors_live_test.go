package recursor

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/dnssec"
)

// ianaRootAnchorsURL is the trust-anchor file the built-in anchors come
// from (DefaultTrustAnchors, dnssec.RootAnchors).
const ianaRootAnchorsURL = "https://data.iana.org/root-anchors/root-anchors.xml"

// ianaAnchors returns the root DS records of an IANA root-anchors.xml
// document that are valid at now, as "<tag> <alg> <digest type> <DIGEST>",
// sorted. A KeyDigest is valid from validFrom until validUntil (absent: no
// end), the same rule RFC 9718 section 2.3 gives relying parties.
func ianaAnchors(doc []byte, now time.Time) ([]string, error) {
	var ta struct {
		Zone    string `xml:"Zone"`
		Digests []struct {
			ValidFrom  string `xml:"validFrom,attr"`
			ValidUntil string `xml:"validUntil,attr"`
			KeyTag     uint16 `xml:"KeyTag"`
			Algorithm  uint8  `xml:"Algorithm"`
			DigestType uint8  `xml:"DigestType"`
			Digest     string `xml:"Digest"`
		} `xml:"KeyDigest"`
	}
	if err := xml.Unmarshal(doc, &ta); err != nil {
		return nil, err
	}
	if ta.Zone != "." {
		return nil, fmt.Errorf("trust anchor zone %q, want \".\"", ta.Zone)
	}
	var out []string
	for _, d := range ta.Digests {
		from, err := time.Parse(time.RFC3339, d.ValidFrom)
		if err != nil {
			return nil, fmt.Errorf("key tag %d: validFrom: %w", d.KeyTag, err)
		}
		if now.Before(from) {
			continue
		}
		if d.ValidUntil != "" {
			until, err := time.Parse(time.RFC3339, d.ValidUntil)
			if err != nil {
				return nil, fmt.Errorf("key tag %d: validUntil: %w", d.KeyTag, err)
			}
			if !now.Before(until) {
				continue
			}
		}
		out = append(out, fmt.Sprintf("%d %d %d %s", d.KeyTag, d.Algorithm, d.DigestType, strings.ToUpper(strings.TrimSpace(d.Digest))))
	}
	slices.Sort(out)
	return out, nil
}

// builtInAnchors renders DS records in the same form as ianaAnchors.
func builtInAnchors(t *testing.T, rrs []dns.RR) []string {
	t.Helper()
	var out []string
	for _, rr := range rrs {
		ds, ok := rr.(*dns.DS)
		if !ok || ds.Hdr.Name != "." {
			t.Fatalf("built-in anchor %v is not a root DS record", rr)
		}
		out = append(out, fmt.Sprintf("%d %d %d %s", ds.KeyTag, ds.Algorithm, ds.DigestType, strings.ToUpper(ds.Digest)))
	}
	slices.Sort(out)
	return out
}

func recursorAnchors(t *testing.T) []string {
	t.Helper()
	ds, err := ParseTrustAnchors(DefaultTrustAnchors)
	if err != nil {
		t.Fatal(err)
	}
	rrs := make([]dns.RR, len(ds))
	for i, d := range ds {
		rrs[i] = d
	}
	return builtInAnchors(t, rrs)
}

// A trimmed copy of root-anchors.xml as IANA published it on November 13,
// 2026 (public keys left out; the comparison uses only the DS fields).
const ianaRootAnchorsFixture = `<?xml version="1.0" encoding="UTF-8"?>
<TrustAnchor id="0C05FDD6-422C-4910-8ED6-430ED15E11C2" source="http://data.iana.org/root-anchors/root-anchors.xml">
    <Zone>.</Zone>
    <KeyDigest id="Kjqmt7v" validFrom="2010-07-15T00:00:00+00:00" validUntil="2019-01-11T00:00:00+00:00">
        <KeyTag>19036</KeyTag>
        <Algorithm>8</Algorithm>
        <DigestType>2</DigestType>
        <Digest>49AAC11D7B6F6446702E54A1607371607A1A41855200FD2CE1CDDE32F24E8FB5</Digest>
    </KeyDigest>
    <KeyDigest id="Klajeyz" validFrom="2017-02-02T00:00:00+00:00">
        <KeyTag>20326</KeyTag>
        <Algorithm>8</Algorithm>
        <DigestType>2</DigestType>
        <Digest>E06D44B80B8F1D39A95C0B0D7C65D08458E880409BBC683457104237C7F8EC8D</Digest>
        <Flags>257</Flags>
    </KeyDigest>
    <KeyDigest id="Kmyv6jo" validFrom="2024-07-18T00:00:00+00:00">
        <KeyTag>38696</KeyTag>
        <Algorithm>8</Algorithm>
        <DigestType>2</DigestType>
        <Digest>683D2D0ACB8C9B712A1948B27F741219298D0A450D612C483AF444A4C0FB2B16</Digest>
        <Flags>257</Flags>
    </KeyDigest>
</TrustAnchor>`

// TestIANAAnchorsValidity checks the validity rule offline: the expired
// KSK-2010 is dropped and both current keys are kept; before KSK-2024's
// validFrom only KSK-2017 (and, then, KSK-2010) is valid.
func TestIANAAnchorsValidity(t *testing.T) {
	got, err := ianaAnchors([]byte(ianaRootAnchorsFixture), time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if want := recursorAnchors(t); !slices.Equal(got, want) {
		t.Fatalf("anchors valid in November 2026 %q, want the built-in %q", got, want)
	}
	got, err = ianaAnchors([]byte(ianaRootAnchorsFixture), time.Date(2018, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !strings.HasPrefix(got[0], "19036 ") || !strings.HasPrefix(got[1], "20326 ") {
		t.Fatalf("anchors valid in 2018 %q, want 19036 and 20326", got)
	}
	if _, err := ianaAnchors([]byte(strings.Replace(ianaRootAnchorsFixture, "<Zone>.</Zone>", "<Zone>com.</Zone>", 1)), time.Now()); err == nil {
		t.Fatal("a trust anchor file for another zone was accepted")
	}
}

// TestBuiltInAnchorsAgree keeps the recursor's and the validator's copies
// of the root anchors identical.
func TestBuiltInAnchorsAgree(t *testing.T) {
	if r, v := recursorAnchors(t), builtInAnchors(t, dnssec.RootAnchors()); !slices.Equal(r, v) {
		t.Fatalf("recursor.DefaultTrustAnchors %q and dnssec.RootAnchors %q differ", r, v)
	}
}

// TestLiveIANATrustAnchors is the release check "the built-in trust anchors
// still match IANA's root-anchors.xml" (dev-docs/release-checklist.md, D170):
// it fetches the file over HTTPS and compares the DS records valid today
// with DefaultTrustAnchors. It does not verify the file's detached CMS
// signature; HTTPS from data.iana.org is the trust here.
func TestLiveIANATrustAnchors(t *testing.T) {
	if testing.Short() || os.Getenv("KUDURRU_LIVE_DNS") == "" {
		t.Skip("set KUDURRU_LIVE_DNS=1 to compare the built-in anchors with IANA's root-anchors.xml")
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(ianaRootAnchorsURL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %s", ianaRootAnchorsURL, resp.Status)
	}
	doc, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	got, err := ianaAnchors(doc, now)
	if err != nil {
		t.Fatal(err)
	}
	want := recursorAnchors(t)
	t.Logf("IANA root anchors valid at %s: %q", now.UTC().Format(time.RFC3339), got)
	if !slices.Equal(got, want) {
		t.Fatalf("IANA's current root anchors %q differ from the built-in %q: update DefaultTrustAnchors and dnssec.RootAnchors", got, want)
	}
}
