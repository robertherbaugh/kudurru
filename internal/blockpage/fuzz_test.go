package blockpage

import (
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"
)

// pageElements are the elements Render writes; a value that adds any
// other element, or any event-handler attribute, escaped badly.
var pageElements = map[string]bool{
	"html": true, "head": true, "meta": true, "title": true, "link": true, "style": true, "body": true,
	"main": true, "div": true, "picture": true, "source": true, "img": true, "h1": true, "h2": true, "p": true,
	"dl": true, "dt": true, "dd": true, "code": true, "footer": true, "a": true, "section": true,
	"form": true, "fieldset": true, "label": true, "input": true, "textarea": true, "button": true,
}

// FuzzBlockPage feeds hostile Host headers, settings and reason texts to
// the block page: normalizeDomain gives "" or a lower-case DNS name and is
// idempotent, formText drops control and bidi characters, and the rendered
// page holds only the template's own elements and attributes, whatever the
// values.
func FuzzBlockPage(f *testing.F) {
	for _, s := range []string{
		"ads.example", "ADS.Example.:8080", "<script>alert(1)</script>", `"><img src=x onerror=alert(1)>`,
		"javascript:alert(1)", "a‮bc", "x\r\ny", "[::1]:80", "1.2.3.4", "_dmarc.example", "-a.example",
		strings.Repeat("a.", 140), "</title><svg/onload=alert(1)>", "&lt;b&gt;", "\x00\x01",
	} {
		f.Add(s, s, s)
	}
	f.Fuzz(func(t *testing.T, host, text, rule string) {
		d := normalizeDomain(host)
		if d != "" {
			if len(d) > 253 || normalizeDomain(d) != d || strings.Trim(d, "abcdefghijklmnopqrstuvwxyz0123456789-_.") != "" {
				t.Fatalf("normalizeDomain(%q) = %q", host, d)
			}
		}
		for _, multiline := range []bool{false, true} {
			v, ok := formText(text, maxReasonLen, multiline)
			if !ok {
				continue
			}
			if !utf8.ValidString(v) || utf8.RuneCountInString(v) > maxReasonLen {
				t.Fatalf("formText(%q) = %q", text, v)
			}
			for _, c := range v {
				if c == '\n' && multiline {
					continue
				}
				if unicode.IsControl(c) || c >= 0x202A && c <= 0x202E || c >= 0x2066 && c <= 0x2069 {
					t.Fatalf("formText(%q) kept %U", text, c)
				}
			}
		}
		p := Page{
			Title: text, Message: text, Domain: d, Client: host, Node: text, Time: time.Unix(0, 0),
			ShowDetails: true, Contact: text, ContactURL: text, DashboardURL: rule,
			Reason:  &Reason{Kind: KindList, List: rule, Rule: rule, Policy: text},
			Request: &RequestView{Action: RequestPath, Flash: text, Status: rule, Note: text, Form: true},
		}
		checkMarkup(t, renderString(p))
	})
}

// checkMarkup fails on an element the template does not write, an
// on* attribute or a link that is not http(s) or mailto:.
func checkMarkup(t *testing.T, page string) {
	t.Helper()
	z := html.NewTokenizer(strings.NewReader(page))
	for {
		switch z.Next() {
		case html.ErrorToken:
			return
		case html.StartTagToken, html.SelfClosingTagToken:
			tok := z.Token()
			if !pageElements[tok.Data] {
				t.Fatalf("unexpected element <%s>", tok.Data)
			}
			for _, a := range tok.Attr {
				if strings.HasPrefix(a.Key, "on") || a.Namespace != "" {
					t.Fatalf("unexpected attribute %s on <%s>", a.Key, tok.Data)
				}
				if a.Key == "href" && tok.Data == "a" {
					l := strings.ToLower(a.Val)
					if !strings.HasPrefix(l, "https://") && !strings.HasPrefix(l, "http://") && !strings.HasPrefix(l, "mailto:") {
						t.Fatalf("unsafe link %q", a.Val)
					}
				}
			}
		}
	}
}
