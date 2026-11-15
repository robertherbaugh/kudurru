package blockpage

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/config"
)

func brandingTestPNG(t *testing.T, width, height int, fill color.RGBA) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			img.SetRGBA(x, y, fill)
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func brandingTestChunk(kind string, data []byte) []byte {
	chunk := make([]byte, len(data)+12)
	binary.BigEndian.PutUint32(chunk[:4], uint32(len(data))) //nolint:gosec // test payloads are bounded to 64 KiB
	copy(chunk[4:8], kind)
	copy(chunk[8:], data)
	binary.BigEndian.PutUint32(chunk[len(chunk)-4:], crc32.ChecksumIEEE(chunk[4:len(chunk)-4]))
	return chunk
}

func brandingTestInsert(raw, chunk []byte) []byte {
	out := bytes.Clone(raw[:len(raw)-12])
	out = append(out, chunk...)
	return append(out, raw[len(raw)-12:]...)
}

func TestBrandingPNGContractAndDetachedBytes(t *testing.T) {
	raw := brandingTestPNG(t, 23, 7, color.RGBA{R: 73, G: 81, B: 99, A: 255})
	// Exercise inclusive byte bounds and metadata retention: safe validated raw
	// files are accepted as-is, independent of a different PNG encoder's output.
	full := brandingTestInsert(raw, brandingTestChunk("tEXt", bytes.Repeat([]byte{'x'}, (64<<10)-len(raw)-12)))
	badCRC := bytes.Clone(raw)
	badCRC[len(badCRC)-1] ^= 1
	badPixels := bytes.Clone(raw)
	for off := 8; off < len(badPixels); {
		n := int(binary.BigEndian.Uint32(badPixels[off : off+4]))
		if string(badPixels[off+4:off+8]) == "IDAT" {
			copy(badPixels[off:off+n+12], brandingTestChunk("IDAT", make([]byte, n)))
			break
		}
		off += n + 12
	}
	for _, tc := range []struct {
		name string
		raw  []byte
		icon bool
		ok   bool
	}{
		{"empty", nil, false, true},
		{"logo", raw, false, true},
		{"full byte bound", full, false, true},
		{"logo dimension bound", brandingTestPNG(t, 1024, 1, color.RGBA{A: 255}), false, true},
		{"icon dimension bound", brandingTestPNG(t, 1, 256, color.RGBA{A: 255}), true, true},
		{"large logo", brandingTestPNG(t, 1025, 1, color.RGBA{A: 255}), false, false},
		{"large icon", brandingTestPNG(t, 1, 257, color.RGBA{A: 255}), true, false},
		{"too many bytes", brandingTestInsert(full, brandingTestChunk("tEXt", nil)), false, false},
		{"truncated signature", raw[:7], false, false},
		{"SVG", []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`), false, false},
		{"URL", []byte("https://example.net/logo.png"), false, false},
		{"data URL instead of bytes", []byte(pngURL(raw)), false, false},
		{"truncated chunk", raw[:len(raw)-1], false, false},
		{"missing end", raw[:len(raw)-12], false, false},
		{"duplicate end", append(bytes.Clone(raw), raw[len(raw)-12:]...), false, false},
		{"trailing payload", append(bytes.Clone(raw), []byte("<script>alert(1)</script>")...), false, false},
		{"nonempty end", append(bytes.Clone(raw[:len(raw)-12]), brandingTestChunk("IEND", []byte{1})...), false, false},
		{"huge chunk length", append(bytes.Clone(raw[:8]), []byte{255, 255, 255, 255, 't', 'E', 'X', 't', 0, 0, 0, 0}...), false, false},
		{"bad CRC", badCRC, false, false},
		{"invalid compressed pixels", badPixels, false, false},
		{"APNG control", brandingTestInsert(raw, brandingTestChunk("acTL", make([]byte, 8))), false, false},
		{"APNG frame", brandingTestInsert(raw, brandingTestChunk("fcTL", make([]byte, 26))), false, false},
		{"APNG data", brandingTestInsert(raw, brandingTestChunk("fdAT", make([]byte, 4))), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := BrandingInput{LogoLight: tc.raw}
			bound := 1024
			if tc.icon {
				in = BrandingInput{Favicon: tc.raw}
				bound = 256
			}
			value := ""
			if len(tc.raw) != 0 {
				value = pngURL(tc.raw)
			}
			_, configErr := config.BrandingPNG(value, bound)
			brand, err := PrepareBranding(in)
			if (err == nil) != tc.ok || (configErr == nil) != tc.ok {
				t.Fatalf("accept=%v: renderer error=%v config error=%v", tc.ok, err, configErr)
			}
			if err != nil || len(tc.raw) == 0 {
				return
			}
			if tc.icon {
				if !bytes.Equal(brand.icon, tc.raw) || brand.iconURL != value {
					t.Fatal("favicon bytes were changed")
				}
			} else if brand.light != value || brand.dark != value {
				t.Fatal("logo bytes changed or single-variant fallback lost")
			}
		})
	}
	original := bytes.Clone(raw)
	brand, err := PrepareBranding(BrandingInput{LogoLight: raw, Favicon: raw})
	if err != nil {
		t.Fatal(err)
	}
	clear(raw)
	if brand.light != pngURL(original) || brand.iconURL != pngURL(original) || !bytes.Equal(brand.icon, original) {
		t.Fatal("prepared appearance retained mutable caller bytes")
	}
}

func TestBrandingNameAndAccentContract(t *testing.T) {
	for _, name := range []string{"", "Acme", strings.Repeat("界", 80), `A <B> & "C"`} {
		if _, err := PrepareBranding(BrandingInput{Name: name}); err != nil {
			t.Fatalf("valid name: %v", err)
		}
	}
	for _, name := range []string{strings.Repeat("a", 81), " A", "A ", "\u2003A", "A\u2003", "A\nB", "A\x00B", "A\u202eB", "A\u200dB", "\xff"} {
		if _, err := PrepareBranding(BrandingInput{Name: name}); err == nil {
			t.Error("invalid name accepted")
		}
	}
	for _, accent := range []string{"", "#123abc", "#000000", "#FFFFFF"} {
		if _, err := PrepareBranding(BrandingInput{AccentColor: accent}); err != nil {
			t.Fatalf("valid accent: %v", err)
		}
	}
	for _, accent := range []string{"red", "#fff", "#GG0011", "#1234567", " #123456", "#123456\n", "#123456;display:none", "url(https://x)", "#12345+"} {
		if _, err := PrepareBranding(BrandingInput{AccentColor: accent}); err == nil {
			t.Errorf("invalid accent accepted: %q", accent)
		}
	}
}

func checkBrandingDocument(t *testing.T, d Document, preview bool) {
	t.Helper()
	body := string(d.Body)
	match := regexp.MustCompile(`(?s)<style>(.*?)</style>`).FindStringSubmatch(body)
	if len(match) != 2 || strings.Count(body, "<style>") != 1 {
		t.Fatal("document must have one stylesheet")
	}
	sum := sha256.Sum256([]byte(match[1]))
	if strings.Count(d.CSP, "'sha256-") != 1 || !strings.Contains(d.CSP, "'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'") {
		t.Fatal("CSP does not match actual stylesheet")
	}
	for _, bad := range []string{"unsafe-inline", "unsafe-eval", "https:", "script-src"} {
		if strings.Contains(d.CSP, bad) {
			t.Errorf("unexpected CSP permission %q", bad)
		}
	}
	frame, form := "'none'", "'self'"
	if preview {
		frame, form = "'self'", "'none'"
	}
	for _, directive := range []string{"default-src 'none'", "base-uri 'none'", "frame-ancestors " + frame, "form-action " + form} {
		if !strings.Contains(d.CSP, directive) {
			t.Errorf("missing CSP directive %q", directive)
		}
	}
	for _, bad := range []string{"<script", " onload=", " onerror=", `style="`, `href="/favicon.ico"`, "http://", "https://"} {
		if strings.Contains(body, bad) {
			t.Errorf("unexpected markup %q", bad)
		}
	}
	checkMarkup(t, body)
}

func TestBrandingDocumentEscapingThemesAndCSP(t *testing.T) {
	light := brandingTestPNG(t, 31, 5, color.RGBA{R: 255, A: 255})
	dark := brandingTestPNG(t, 5, 31, color.RGBA{B: 255, A: 255})
	name := `Acme "><script>alert(1)</script>`
	for _, accent := range []string{"", "#19aACC", "#FFFFFF", "#000000", "#ffff00"} {
		brand, err := PrepareBranding(BrandingInput{Name: name, LogoLight: light, LogoDark: dark, Favicon: light, AccentColor: accent})
		if err != nil {
			t.Fatal(err)
		}
		for _, preview := range []bool{false, true} {
			doc, err := RenderDocument(Page{Branding: brand, Domain: "blocked.example", Time: time.Unix(1, 0), Preview: preview})
			if err != nil {
				t.Fatal(err)
			}
			checkBrandingDocument(t, doc, preview)
			body := string(doc.Body)
			for _, want := range []string{`alt="` + esc(name) + `"`, "Blocked by " + esc(name) + " DNS.", `src="` + pngURL(light) + `"`, `srcset="` + pngURL(dark) + `"`, `href="` + pngURL(light) + `"`, "object-fit:contain"} {
				if !strings.Contains(body, want) {
					t.Errorf("page lacks %q", want)
				}
			}
			if accent != "" && !strings.Contains(body, "--clay:"+strings.ToUpper(accent)) {
				t.Error("configured accent not used")
			}
		}
	}
	for _, accent := range []color.RGBA{{A: 255}, {R: 255, G: 255, B: 255, A: 255}, {R: 255, G: 255, A: 255}, {R: 1, B: 120, A: 255}} {
		for _, theme := range []struct {
			surface color.RGBA
			lighter bool
		}{{color.RGBA{R: 247, G: 243, B: 234, A: 255}, false}, {color.RGBA{R: 24, G: 37, B: 34, A: 255}, true}} {
			shade, err := brandingColor(brandingContrast(accent, theme.surface, theme.lighter))
			if err != nil {
				t.Fatal(err)
			}
			a, b := brandingLuminance(shade), brandingLuminance(theme.surface)
			if (max(a, b)+0.05)/(min(a, b)+0.05) < 4.5 {
				t.Fatal("derived text accent has insufficient contrast")
			}
		}
	}
}
