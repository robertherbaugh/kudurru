package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/robertherbaugh/kudurru/internal/auth/providerresources"
	"github.com/robertherbaugh/kudurru/internal/config"
)

// loadProviderTrust captures actual certificate bytes before acquiring any
// publication/resource/host/store guard. It never substitutes subject names from
// SystemCertPool, consults SSL_CERT_FILE/DIR, or leaves a live trust-pool fallback.
func loadProviderTrust(ctx context.Context, policy config.ProviderResources) ([]byte, error) {
	return readProviderTrust(ctx, policy, providerSystemBundles(runtime.GOOS))
}

func providerSystemBundles(platform string) []string {
	switch platform {
	case "linux":
		return []string{"/etc/ssl/certs/ca-certificates.crt", "/etc/pki/tls/certs/ca-bundle.crt", "/etc/ssl/ca-bundle.pem", "/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem", "/etc/ssl/cert.pem"}
	case "darwin":
		return []string{"/etc/ssl/cert.pem"}
	case "freebsd", "openbsd", "netbsd", "dragonfly":
		return []string{"/etc/ssl/cert.pem", "/usr/local/share/certs/ca-root-nss.crt", "/etc/ssl/certs/ca-certificates.crt"}
	default:
		return nil
	}
}

func readProviderTrust(ctx context.Context, policy config.ProviderResources, systemPaths []string) ([]byte, error) {
	if ctx == nil || len(policy.CAFiles) > 32 {
		return nil, errors.New("provider trust: invalid input")
	}
	remaining := int64(providerresources.MaxTrustBytes)
	var bundles [][]byte
	read := func(path string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		f, err := os.Open(path) //nolint:gosec // G304: explicit trusted host path or fixed platform bundle
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		stat, err := f.Stat()
		if err != nil {
			return err
		}
		if !stat.Mode().IsRegular() || stat.Size() > remaining {
			return providerresources.ErrBounds
		}
		data, err := io.ReadAll(io.LimitReader(f, remaining+1))
		if err != nil {
			return err
		}
		if int64(len(data)) > remaining {
			return providerresources.ErrBounds
		}
		remaining -= int64(len(data))
		bundles = append(bundles, data)
		return ctx.Err()
	}
	if policy.SystemTrust {
		found := false
		for _, path := range systemPaths {
			err := read(path)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("provider trust: system bundle: %w", err)
			}
			found = true
			break
		}
		if !found {
			return nil, errors.New("provider trust: no supported system CA bundle; configure explicit CA files")
		}
	}
	for _, path := range policy.CAFiles {
		if len(path) > 4096 || !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsRune(path, 0) {
			return nil, errors.New("provider trust: invalid CA file")
		}
		if err := read(path); err != nil {
			return nil, fmt.Errorf("provider trust: CA file: %w", err)
		}
	}
	trust, err := canonicalProviderTrust(bundles)
	if err != nil {
		return nil, err
	}
	if len(trust) == 0 && (policy.SystemTrust || len(policy.CAFiles) > 0 || len(policy.AllowedOrigins) > 0) {
		return nil, errors.New("provider trust: no CA certificates")
	}
	return trust, ctx.Err()
}

func canonicalProviderTrust(bundles [][]byte) ([]byte, error) {
	invalid := errors.New("provider trust: malformed or unsupported CA bundle")
	remaining := providerresources.MaxTrustBytes
	for _, b := range bundles {
		if len(b) > remaining {
			return nil, providerresources.ErrBounds
		}
		remaining -= len(b)
	}
	certificates := map[string][]byte{}
	for _, data := range bundles {
		for len(data) > 0 {
			data = bytes.TrimSpace(data)
			if len(data) == 0 {
				break
			}
			// Standard bundle generators include human-readable # comments.
			// Other text, private keys and malformed PEM cannot be skipped.
			if data[0] == '#' {
				_, data, _ = bytes.Cut(data, []byte("\n"))
				continue
			}
			if !bytes.HasPrefix(data, []byte("-----BEGIN CERTIFICATE-----\n")) && !bytes.HasPrefix(data, []byte("-----BEGIN CERTIFICATE-----\r\n")) {
				return nil, invalid
			}
			end := bytes.Index(data, []byte("-----END CERTIFICATE-----"))
			if end < 0 {
				return nil, invalid
			}
			end += len("-----END CERTIFICATE-----")
			block, extra := pem.Decode(data[:end])
			if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 || len(bytes.TrimSpace(extra)) != 0 {
				return nil, invalid
			}
			cert, err := x509.ParseCertificate(block.Bytes)
			if err != nil || !cert.IsCA || !cert.BasicConstraintsValid {
				return nil, invalid
			}
			digest := sha256.Sum256(block.Bytes)
			key := hex.EncodeToString(digest[:])
			if _, exists := certificates[key]; !exists {
				if len(certificates) >= providerresources.MaxTrustCertificates {
					return nil, providerresources.ErrBounds
				}
				certificates[key] = block.Bytes
			}
			data = data[end:]
		}
	}
	keys := make([]string, 0, len(certificates))
	for key := range certificates {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	var out bytes.Buffer
	for _, key := range keys {
		encoded := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificates[key]})
		if len(encoded) > providerresources.MaxTrustBytes-out.Len() {
			return nil, providerresources.ErrBounds
		}
		_, _ = out.Write(encoded)
	}
	return out.Bytes(), nil
}
