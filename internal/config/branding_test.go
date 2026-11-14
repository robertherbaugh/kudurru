package config

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/png"
	"strings"
	"testing"
)

func brandingTestPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewNRGBA(image.Rect(0, 0, width, height))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestBrandingValidation(t *testing.T) {
	valid := brandingPNGPrefix + base64.StdEncoding.EncodeToString(brandingTestPNG(t, 2, 3))
	for _, b := range []Branding{{}, {Name: "Acme & Partners", LogoLight: valid, LogoDark: valid, Favicon: valid, AccentColor: "#123aBC"}, {Name: strings.Repeat("界", 80)}} {
		if err := b.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	for name, b := range map[string]Branding{
		"long name": {Name: strings.Repeat("界", 81)}, "control": {Name: "A\nB"}, "format": {Name: "A\u202eB"}, "space": {Name: " A "}, "utf8": {Name: "\xff"},
		"color injection": {AccentColor: "#123456;display:none"}, "short color": {AccentColor: "#fff"},
		"remote image": {LogoLight: "https://example.test/logo.png"}, "SVG": {LogoDark: "data:image/svg+xml;base64,PHN2Zz4="},
		"malformed": {Favicon: "data:image/png;base64,AAAA"}, "large favicon": {Favicon: brandingPNGPrefix + base64.StdEncoding.EncodeToString(brandingTestPNG(t, 257, 1))},
		"large logo": {LogoLight: brandingPNGPrefix + base64.StdEncoding.EncodeToString(brandingTestPNG(t, 1025, 1))},
		"huge data":  {LogoLight: brandingPNGPrefix + strings.Repeat("A", base64.StdEncoding.EncodedLen(BrandingImageBytes)+4)},
	} {
		t.Run(name, func(t *testing.T) {
			if err := b.Validate(); err == nil {
				t.Fatal("invalid branding accepted")
			}
		})
	}
}

func TestBrandingRejectsPNGAnimationCorruptionAndTrailingData(t *testing.T) {
	raw := brandingTestPNG(t, 2, 3)
	chunk := make([]byte, 20)
	binary.BigEndian.PutUint32(chunk, 8)
	copy(chunk[4:], "acTL")
	binary.BigEndian.PutUint32(chunk[8:], 1)
	binary.BigEndian.PutUint32(chunk[16:], crc32.ChecksumIEEE(chunk[4:16]))
	animated := append(bytes.Clone(raw[:33]), chunk...)
	animated = append(animated, raw[33:]...)
	corrupt := bytes.Clone(raw)
	corrupt[len(corrupt)-1] ^= 1
	for _, value := range [][]byte{animated, corrupt, raw[:len(raw)-1], append(bytes.Clone(raw), []byte("trailing")...)} {
		if _, err := BrandingPNG(brandingPNGPrefix+base64.StdEncoding.EncodeToString(value), 1024); err == nil {
			t.Fatal("invalid PNG accepted")
		}
	}
	encoded := base64.StdEncoding.EncodeToString(raw)
	if _, err := BrandingPNG(brandingPNGPrefix+encoded[:4]+"\n"+encoded[4:], 1024); err == nil {
		t.Fatal("noncanonical data URL accepted")
	}
}
