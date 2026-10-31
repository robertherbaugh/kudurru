package blockpage

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"image/color"
	"image/png"
	"math"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// BrandingInput contains presentation data only. Assets are decoded PNG file
// bytes, not URLs or base64 text. Entitlement and captured-source selection
// belong to the caller; preparing an appearance grants no licensed capability.
type BrandingInput struct {
	Name                         string
	LogoLight, LogoDark, Favicon []byte
	AccentColor                  string
}

// Branding is immutable, bounded presentation prepared before publication.
// It retains no input slices, file handles, callbacks or license state.
type Branding struct {
	name, light, dark, iconURL string
	icon                       []byte
	iconType                   string
	css, csp, previewCSP       string
}

// Document binds the response policy to the CSS in the same rendered body.
type Document struct {
	Body []byte
	CSP  string
}

var builtInBranding = &Branding{
	name: "kudurru", light: logoLightURL, dark: logoDarkURL,
	iconURL: "data:image/x-icon;base64," + base64.StdEncoding.EncodeToString(favicon),
	icon:    favicon, iconType: "image/x-icon", css: css, csp: CSP, previewCSP: PreviewCSP,
}

func actualBranding(brand *Branding) *Branding {
	if brand == nil || brand.name == "" || brand.css == "" || brand.csp == "" || brand.previewCSP == "" || brand.light == "" || brand.dark == "" || brand.iconURL == "" {
		return builtInBranding
	}
	return brand
}

// PrepareBranding validates and detaches a projected appearance. PNG files are
// capped at 64 KiB and 1024 pixels per dimension (256 for the favicon), fully
// decoded and checked for animation and trailing bytes. Validated bytes are
// retained unchanged in a detached copy. A missing logo variant uses the supplied one.
// Empty input returns the built-in appearance. No network or file I/O occurs.
func PrepareBranding(in BrandingInput) (*Branding, error) {
	if len(in.Name) > 80*utf8.UTFMax || !utf8.ValidString(in.Name) || utf8.RuneCountInString(in.Name) > 80 || strings.TrimSpace(in.Name) != in.Name || strings.ContainsFunc(in.Name, func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }) {
		return nil, errors.New("blockpage: invalid branding name")
	}
	out := *builtInBranding
	if in.Name != "" {
		out.name = in.Name
	}
	light, err := brandingPNG(in.LogoLight, 1024)
	if err != nil {
		return nil, fmt.Errorf("blockpage: light logo: %w", err)
	}
	dark, err := brandingPNG(in.LogoDark, 1024)
	if err != nil {
		return nil, fmt.Errorf("blockpage: dark logo: %w", err)
	}
	if len(light) != 0 || len(dark) != 0 {
		if len(light) == 0 {
			light = dark
		}
		if len(dark) == 0 {
			dark = light
		}
		out.light, out.dark = pngURL(light), pngURL(dark)
	}
	icon, err := brandingPNG(in.Favicon, 256)
	if err != nil {
		return nil, fmt.Errorf("blockpage: favicon: %w", err)
	}
	if len(icon) != 0 {
		out.icon, out.iconType, out.iconURL = icon, "image/png", pngURL(icon)
	}
	if in.AccentColor != "" {
		accent, err := brandingColor(in.AccentColor)
		if err != nil {
			return nil, err
		}
		// The exact requested accent remains the border. Foreground/button
		// colors use shades that meet AA against both theme surfaces.
		light := brandingContrast(accent, color.RGBA{R: 247, G: 243, B: 234, A: 255}, false)
		dark := brandingContrast(accent, color.RGBA{R: 24, G: 37, B: 34, A: 255}, true)
		out.css = strings.Replace(css, "--clay:#B65C3A", "--clay:"+strings.ToUpper(in.AccentColor), 1)
		out.css = strings.Replace(out.css, "--accent:#9A4A2C", "--accent:"+light+";--accent-ink:#FFFFFF", 1)
		out.css = strings.Replace(out.css, "--accent:#E08A66", "--accent:"+dark+";--accent-ink:#101917", 1)
		out.css = strings.Replace(out.css, "color:#FFFFFF;background:var(--accent)", "color:var(--accent-ink);background:var(--accent)", 1)
		out.css = strings.Replace(out.css, "color:var(--card);background:var(--clay)", "color:var(--accent-ink);background:var(--accent)", 1)
		out.csp, out.previewCSP = brandingCSP(out.css, false), brandingCSP(out.css, true)
	}
	return &out, nil
}

func brandingPNG(raw []byte, maxDimension int) ([]byte, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if len(raw) > 64<<10 {
		return nil, errors.New("PNG exceeds 64 KiB")
	}
	if len(raw) < 8 || string(raw[:8]) != "\x89PNG\r\n\x1a\n" {
		return nil, errors.New("invalid PNG signature")
	}
	ended := false
	for rest := raw[8:]; len(rest) != 0; {
		if len(rest) < 12 {
			return nil, errors.New("invalid PNG chunk length")
		}
		n := uint64(binary.BigEndian.Uint32(rest[:4])) + 12
		if n > uint64(len(rest)) {
			return nil, errors.New("invalid PNG chunk length")
		}
		kind := string(rest[4:8])
		if kind == "acTL" || kind == "fcTL" || kind == "fdAT" {
			return nil, errors.New("animated PNG is not supported")
		}
		rest = rest[n:]
		if kind == "IEND" {
			if n != 12 || len(rest) != 0 {
				return nil, errors.New("invalid PNG ending")
			}
			ended = true
		}
	}
	if !ended {
		return nil, errors.New("missing PNG ending")
	}
	config, err := png.DecodeConfig(bytes.NewReader(raw))
	if err != nil || config.Width < 1 || config.Height < 1 || config.Width > maxDimension || config.Height > maxDimension {
		return nil, errors.New("invalid PNG dimensions or format")
	}
	if _, err := png.Decode(bytes.NewReader(raw)); err != nil {
		return nil, errors.New("invalid PNG data")
	}
	return bytes.Clone(raw), nil
}

func pngURL(raw []byte) string {
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(raw)
}

func brandingColor(value string) (color.RGBA, error) {
	if len(value) != 7 || value[0] != '#' {
		return color.RGBA{}, errors.New("blockpage: accent must be #RRGGBB")
	}
	n, err := strconv.ParseUint(value[1:], 16, 24)
	if err != nil {
		return color.RGBA{}, errors.New("blockpage: accent must be #RRGGBB")
	}
	return color.RGBA{R: byte((n >> 16) & 0xff), G: byte((n >> 8) & 0xff), B: byte(n & 0xff), A: 255}, nil
}

func brandingLuminance(c color.RGBA) float64 {
	linear := func(channel uint8) float64 {
		v := float64(channel) / 255
		if v <= 0.04045 {
			return v / 12.92
		}
		return math.Pow((v+0.055)/1.055, 2.4)
	}
	return 0.2126*linear(c.R) + 0.7152*linear(c.G) + 0.0722*linear(c.B)
}

func brandingContrast(accent, surface color.RGBA, lighter bool) string {
	background := brandingLuminance(surface)
	for step := 0; step <= 255; step++ {
		mix := func(channel uint8) uint8 {
			if lighter {
				return uint8(int(channel) + (255-int(channel))*step/255) //nolint:gosec // bounded interpolation
			}
			return uint8(int(channel) * (255 - step) / 255) //nolint:gosec // bounded interpolation
		}
		c := color.RGBA{R: mix(accent.R), G: mix(accent.G), B: mix(accent.B), A: 255}
		foreground := brandingLuminance(c)
		contrast := (max(foreground, background) + 0.05) / (min(foreground, background) + 0.05)
		if contrast >= 4.5 {
			return fmt.Sprintf("#%02X%02X%02X", c.R, c.G, c.B)
		}
	}
	return "#000000" // unreachable: pure black/white always meets the target
}

func brandingCSP(style string, preview bool) string {
	sum := sha256.Sum256([]byte(style))
	hash := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
	policy := "default-src 'none'; style-src " + hash + "; img-src 'self' data:; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"
	if preview {
		policy = "default-src 'none'; style-src " + hash + "; img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'self'"
	}
	return policy
}
