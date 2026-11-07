package externaldns

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"syscall"
)

const maxTokenFileBytes = 4 << 10

var errTokenFile = errors.New("externaldns: invalid externalDNS.webhook.tokenFile: need a readable regular file of at most 4 KiB holding a bearer token of at least 16 characters")

// webhookToken captures a startup credential without retaining its plaintext.
// It is transport authentication only; dispatch still enforces write authority.
type webhookToken struct{ digest [sha256.Size]byte }

func loadWebhookToken(path string) (*webhookToken, error) {
	if path == "" {
		return nil, nil
	}
	// Like the credential resolver, follow operator-selected mounted-secret
	// symlinks but reject special files without blocking on a FIFO writer.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0) //nolint:gosec // G304: trusted host-only secret file, never a public object reference
	if err != nil {
		return nil, errTokenFile
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o444 == 0 || info.Size() > maxTokenFileBytes {
		return nil, errTokenFile
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxTokenFileBytes+1))
	defer clear(raw)
	if err != nil || len(raw) > maxTokenFileBytes {
		return nil, errTokenFile
	}
	token := bytes.TrimSpace(raw)
	if len(token) < 16 || !validBearerToken(token) {
		return nil, errTokenFile
	}
	return &webhookToken{digest: sha256.Sum256(token)}, nil
}

// validBearerToken accepts RFC 6750 b64token syntax, including unpadded hex
// or base64url secrets. Whitespace, controls and comma-joined credentials fail.
func validBearerToken(token []byte) bool {
	if len(token) == 0 || len(token) > maxTokenFileBytes {
		return false
	}
	padding := false
	for _, b := range token {
		if b == '=' {
			padding = true
			continue
		}
		allowed := 'a' <= b && b <= 'z' || 'A' <= b && b <= 'Z' || '0' <= b && b <= '9' || strings.ContainsRune("-._~+/", rune(b))
		if padding || !allowed {
			return false
		}
	}
	return token[0] != '='
}

func (t *webhookToken) accepts(header http.Header) bool {
	values := header.Values("Authorization")
	if len(values) != 1 {
		return false
	}
	scheme, token, ok := strings.Cut(values[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || !validBearerToken([]byte(token)) {
		return false
	}
	got := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(got[:], t.digest[:]) == 1
}
