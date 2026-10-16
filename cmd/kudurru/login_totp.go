package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/robertherbaugh/kudurru/internal/apiclient"
)

type loginFactorOptions struct {
	username, password, token string
	passwordStdin, tokenStdin bool
	prompt, stdin             bool
}

func (f loginFactorOptions) validate() error {
	if !f.prompt && !f.stdin {
		return nil // Preserve existing no-factor credential resolution.
	}
	switch {
	case f.prompt && f.stdin:
		return errors.New("use either --totp or --totp-stdin")
	case f.username == "" || f.token != "" || f.tokenStdin:
		return errors.New("authenticator input requires --username and cannot be combined with token input")
	case f.passwordStdin && (f.prompt || f.password != ""):
		return errors.New("--password-stdin requires --totp-stdin and cannot be combined with --password in factor mode")
	case f.stdin && !f.passwordStdin && f.password == "":
		return errors.New("--totp-stdin requires --password-stdin or --password")
	}
	return nil
}

func (o *cliOpts) readLoginProof(cmd *cobra.Command, f loginFactorOptions) (string, string, error) {
	if err := cmd.Context().Err(); err != nil {
		return "", "", err
	}
	if !f.prompt && !f.stdin {
		password, err := o.readPassword(cmd, f.password, f.passwordStdin, f.username)
		if err != nil {
			return "", "", errors.New("could not read password; empty password or input unavailable")
		}
		return password, "", nil
	}
	if err := f.validate(); err != nil {
		return "", "", err
	}
	if f.stdin {
		// One reader owns both lines; a second reader can discard a prefetched
		// code when the password reader goes out of scope.
		r := bufio.NewReaderSize(o.stdin, 1026)
		password := f.password
		var err error
		if f.passwordStdin {
			password, err = readLoginLine(r, 1024)
			if err != nil {
				return "", "", errors.New("could not read password; empty password or input exceeds limit")
			}
		}
		code, err := readLoginLine(r, 6)
		if err != nil || !validLoginCode(code) {
			return "", "", errors.New("authenticator code must contain six ASCII digits")
		}
		if err := validateLoginPassword(password); err != nil {
			return "", "", err
		}
		if err := cmd.Context().Err(); err != nil {
			return "", "", err
		}
		return password, code, nil
	}
	file, ok := o.stdin.(*os.File)
	if !ok || !term.IsTerminal(int(file.Fd())) { //nolint:gosec // File descriptors fit in int.
		return "", "", errors.New("--totp requires a terminal; use --totp-stdin with an explicit password source")
	}
	return readTerminalLoginProof(cmd.Context(), cmd.ErrOrStderr(), f.password, func() ([]byte, error) {
		return term.ReadPassword(int(file.Fd())) //nolint:gosec // File descriptors fit in int.
	})
}

func readTerminalLoginProof(ctx context.Context, out io.Writer, password string, hidden func() ([]byte, error)) (string, string, error) {
	read := func(prompt string) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		_, _ = io.WriteString(out, prompt)
		value, err := hidden()
		defer clear(value)
		_, _ = io.WriteString(out, "\n")
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if err != nil {
			return "", errors.New("hidden credential input unavailable")
		}
		return string(value), nil
	}
	var err error
	if password == "" {
		password, err = read("Password: ")
		if err != nil {
			return "", "", err
		}
	}
	if err := validateLoginPassword(password); err != nil {
		return "", "", err
	}
	code, err := read("Authenticator code (6 digits): ")
	if err != nil {
		return "", "", err
	}
	if !validLoginCode(code) {
		return "", "", errors.New("authenticator code must contain six ASCII digits")
	}
	return password, code, nil
}

// readLoginLine has a fixed allocation bound and consumes one LF/CRLF line.
// EOF may terminate a nonempty line. It never waits for whole-stream EOF.
func readLoginLine(r *bufio.Reader, limit int) (string, error) {
	line := make([]byte, 0, limit+1)
	defer func() { clear(line[:cap(line)]) }()
	for {
		b, err := r.ReadByte()
		if err != nil && !errors.Is(err, io.EOF) {
			return "", errors.New("credential input unavailable")
		}
		if err != nil || b == '\n' {
			if len(line) > 0 && line[len(line)-1] == '\r' && b == '\n' && err == nil {
				line = line[:len(line)-1]
			}
			if len(line) == 0 || len(line) > limit {
				return "", errors.New("credential input empty or exceeds limit")
			}
			return string(line), nil
		}
		if len(line) == limit+1 {
			return "", errors.New("credential input exceeds limit")
		}
		line = append(line, b)
	}
}

func validateLoginPassword(password string) error {
	if password == "" || len(password) > 1024 || strings.ContainsAny(password, "\r\n") {
		return errors.New("password must contain 1 to 1024 bytes without line delimiters")
	}
	return nil
}

func validLoginCode(code string) bool {
	if len(code) != 6 {
		return false
	}
	for i := range len(code) {
		if code[i] < '0' || code[i] > '9' {
			return false
		}
	}
	return true
}

// loginStageError covers all local credential-chain stages, including GETs.
// It never formats or retains untrusted transport/server diagnostic text.
func loginStageError(stage string, err error) error {
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("%s: %w", stage, context.Canceled)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s: %w", stage, context.DeadlineExceeded)
	}
	var response *apiclient.Error
	if errors.As(err, &response) && response.StatusCode >= 100 && response.StatusCode <= 599 {
		message := "request failed; outcome may be unknown; no automatic retry"
		if response.StatusCode == http.StatusUnauthorized && stage == "login" {
			message = "invalid username or password or authenticator code"
		}
		return fmt.Errorf("%s: %w", stage, &apiclient.Error{StatusCode: response.StatusCode, Message: message})
	}
	return fmt.Errorf("%s: response unavailable or invalid; outcome may be unknown; no automatic retry", stage)
}
