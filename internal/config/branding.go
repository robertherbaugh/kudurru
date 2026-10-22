package config

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"image/png"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Branding holds public presentation settings. Saving these values does not
// grant an entitlement: every renderer separately requires Enterprise branding.
type Branding struct {
	Name        string `koanf:"name" json:"name"`
	LogoLight   string `koanf:"logoLight" json:"logoLight"`
	LogoDark    string `koanf:"logoDark" json:"logoDark"`
	Favicon     string `koanf:"favicon" json:"favicon"`
	AccentColor string `koanf:"accentColor" json:"accentColor"`
}

// BrandingImageBytes bounds each embedded, static PNG before image decoding.
const BrandingImageBytes = 64 << 10

const brandingPNGPrefix = "data:image/png;base64,"

// BrandingImageDataURLBytes is the exact canonical data URL bound for one PNG.
// Admission schemas use the same limit as the full image validator below.
const BrandingImageDataURLBytes = len(brandingPNGPrefix) + ((BrandingImageBytes + 2) / 3 * 4)

var brandingColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// Validate checks presentation data without consulting licensing or the network.
// Errors never include image bytes or untrusted setting values.
func (b Branding) Validate() error {
	var errs []error
	if !utf8.ValidString(b.Name) || utf8.RuneCountInString(b.Name) > 80 || strings.TrimSpace(b.Name) != b.Name || strings.ContainsFunc(b.Name, func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }) {
		errs = append(errs, errors.New("branding.name: use at most 80 characters without surrounding whitespace or control characters"))
	}
	if b.AccentColor != "" && !brandingColor.MatchString(b.AccentColor) {
		errs = append(errs, errors.New("branding.accentColor: use an empty value or a six-digit #RRGGBB color"))
	}
	for _, asset := range []struct {
		name, value string
		dimension   int
	}{{"logoLight", b.LogoLight, 1024}, {"logoDark", b.LogoDark, 1024}, {"favicon", b.Favicon, 256}} {
		if _, err := BrandingPNG(asset.value, asset.dimension); err != nil {
			errs = append(errs, fmt.Errorf("branding.%s: %w", asset.name, err))
		}
	}
	return errors.Join(errs...)
}

// BrandingPNG decodes one bounded static PNG data URL. Empty selects built-in
// artwork. URLs, SVG, animation, trailing payloads and oversized images fail.
func BrandingPNG(value string, dimension int) ([]byte, error) {
	if value == "" {
		return nil, nil
	}
	invalid := errors.New("use a static PNG data URL, at most 64 KiB, within the image dimension limit")
	if dimension < 1 || dimension > 1024 || len(value) > BrandingImageDataURLBytes || !strings.HasPrefix(value, brandingPNGPrefix) {
		return nil, invalid
	}
	encoded := strings.TrimPrefix(value, brandingPNGPrefix)
	data, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(data) > BrandingImageBytes || base64.StdEncoding.EncodeToString(data) != encoded || len(data) < 8 || string(data[:8]) != "\x89PNG\r\n\x1a\n" {
		return nil, invalid
	}
	// Reject animation and data after IEND before passing bytes to an image
	// decoder. DecodeConfig bounds the allocation; Decode verifies all pixels.
	ended := false
	for rest := data[8:]; len(rest) != 0; {
		if len(rest) < 12 {
			return nil, invalid
		}
		n := uint64(binary.BigEndian.Uint32(rest[:4])) + 12
		if n > uint64(len(rest)) {
			return nil, invalid
		}
		kind := string(rest[4:8])
		if kind == "acTL" || kind == "fcTL" || kind == "fdAT" {
			return nil, invalid
		}
		rest = rest[n:]
		if kind == "IEND" {
			if n != 12 || len(rest) != 0 {
				return nil, invalid
			}
			ended = true
		}
	}
	if !ended {
		return nil, invalid
	}
	info, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil || info.Width < 1 || info.Height < 1 || info.Width > dimension || info.Height > dimension {
		return nil, invalid
	}
	if _, err := png.Decode(bytes.NewReader(data)); err != nil {
		return nil, invalid
	}
	return data, nil
}
