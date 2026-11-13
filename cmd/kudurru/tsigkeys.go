package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/apiclient"
	"github.com/robertherbaugh/kudurru/internal/cli"
	"github.com/robertherbaugh/kudurru/internal/tsigkeys"
)

// maxTSIGSecretFile bounds the secret file read; a canonical secret of the
// largest supported size is 684 base64 characters.
const maxTSIGSecretFile = 4096

// newTSIGKeysCmd is `kudurru tsigkeys`: stored TSIG key management through
// the /tsigkeys routes (D394). Secrets are read only from a file or stdin,
// never from an argument, and no output contains one.
func newTSIGKeysCmd(o *cliOpts) *cobra.Command {
	root := &cobra.Command{Use: "tsigkeys", Short: "Review and manage stored TSIG keys", Long: `Review and manage stored TSIG keys on complete SQLite and Kubernetes nodes.
Requires tsigkeys.read, tsigkeys.write or tsigkeys.delete (admin by default).

The secret is read from --secret-file (a path, or - for stdin), never from an
argument. The file holds the standard base64 secret of at most 512 bytes;
whitespace and line breaks are ignored. No command prints a secret: the server
never returns one, so keep your own copy for the peers that share the key.

Create requires an absent stored name. Replace, reseal and revoke require the
exact UID and resourceVersion from a separately reviewed list. File keys are
read-only. Each command is sent once. A refused command, a missing response or
an uncertain outcome is never retried or refreshed for you: run list and review
the fresh metadata before any further action. Exit status 0 with "published:
false" (HTTP 202) means the change committed but the node could not confirm
its publication in time; it was not rolled back.`}
	root.AddCommand(&cobra.Command{Use: "list", Short: "List file and stored TSIG key metadata", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, _, err := o.client()
		if err != nil {
			return err
		}
		var list api.TSIGKeyList
		if err := c.Get(cmd.Context(), "/tsigkeys", nil, &list); err != nil {
			return err
		}
		return o.render(cmd, list, func(bool) (*cli.Table, error) {
			table := &cli.Table{Headers: []string{"NAME", "ALGORITHM", "SOURCE", "UID", "RESOURCEVERSION", "CREATED"}}
			for _, key := range list.Items {
				created := "-"
				if key.CreatedAt != nil {
					created = formatTime(*key.CreatedAt)
				}
				table.Add(key.KeyName, key.Algorithm, key.Source, orDash(key.UID), orDash(key.ResourceVersion), created)
			}
			return table, nil
		})
	}})
	for _, operation := range []api.TSIGKeyOperation{api.TSIGKeyCreate, api.TSIGKeyReplace, api.TSIGKeyReseal, api.TSIGKeyRevoke} {
		var secretFile, algorithm, uid, rv string
		sub := &cobra.Command{
			Use:   string(operation) + " NAME",
			Short: tsigKeyCommandHelp[operation],
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				name := args[0]
				if name == "" || len(name) > 254 || strings.ContainsAny(name, "/\\\x00\r\n\t ") {
					return errors.New("NAME must be one TSIG key name of at most 254 characters")
				}
				if operation != api.TSIGKeyCreate && (uid == "" || len(uid) > 256 || rv == "" || len(rv) > 256) {
					return errors.New("exact --uid and --resource-version from the reviewed key are required")
				}
				if operation == api.TSIGKeyCreate && algorithm == "" {
					return errors.New("--algorithm is required (hmac-sha256 or hmac-sha512)")
				}
				var secret []byte
				if operation == api.TSIGKeyCreate || operation == api.TSIGKeyReplace {
					var err error
					if secret, err = readTSIGSecret(secretFile, o.stdin); err != nil {
						return err
					}
					defer clear(secret)
				}
				body, err := tsigKeyCommandBody(operation, name, algorithm, uid, rv, secret)
				if err != nil {
					return err
				}
				defer clear(body)
				c, _, err := o.client()
				if err != nil {
					return err
				}
				keyName := name
				if operation == api.TSIGKeyCreate {
					keyName = ""
				}
				status, raw, err := c.TSIGKeyCommand(cmd.Context(), string(operation), keyName, body)
				if err != nil {
					return tsigKeyCommandError(operation, err)
				}
				var receipt api.TSIGKeyCommandReceipt
				if json.Unmarshal(raw, &receipt) != nil || !receipt.Committed || receipt.Operation != operation ||
					receipt.Removed != (operation == api.TSIGKeyRevoke) || receipt.Published != (status != http.StatusAccepted) ||
					receipt.Key.KeyName == "" || receipt.Key.Source != "store" || operation != api.TSIGKeyCreate && receipt.Key.UID != uid {
					return tsigKeyCommandError(operation, apiclient.ErrTSIGKeyOutcomeUnknown)
				}
				if !receipt.Published {
					_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "%s committed (HTTP 202): publication was not confirmed in time; this is not a rollback. Run kudurru tsigkeys list before any further action.\n", operation)
				}
				return o.render(cmd, receipt, func(bool) (*cli.Table, error) {
					table := &cli.Table{Headers: []string{"OPERATION", "NAME", "ALGORITHM", "UID", "RESOURCEVERSION", "COMMITTED", "PUBLISHED", "REMOVED"}}
					table.Add(string(receipt.Operation), receipt.Key.KeyName, receipt.Key.Algorithm, receipt.Key.UID, receipt.Key.ResourceVersion,
						fmt.Sprint(receipt.Committed), fmt.Sprint(receipt.Published), fmt.Sprint(receipt.Removed))
					return table, nil
				})
			},
		}
		switch operation {
		case api.TSIGKeyCreate:
			sub.Flags().StringVar(&algorithm, "algorithm", "", "required: hmac-sha256 or hmac-sha512")
		case api.TSIGKeyReplace:
			sub.Flags().StringVar(&algorithm, "algorithm", "", "new algorithm (default: keep the current one)")
		}
		if operation == api.TSIGKeyCreate || operation == api.TSIGKeyReplace {
			sub.Flags().StringVar(&secretFile, "secret-file", "", "required: file holding the base64 secret, or - for stdin")
		}
		if operation != api.TSIGKeyCreate {
			sub.Flags().StringVar(&uid, "uid", "", "required: exact reviewed key UID")
			sub.Flags().StringVar(&rv, "resource-version", "", "required: exact reviewed key resourceVersion")
		}
		root.AddCommand(sub)
	}
	return root
}

var tsigKeyCommandHelp = map[api.TSIGKeyOperation]string{
	api.TSIGKeyCreate:  "Create one absent stored TSIG key from a secret file",
	api.TSIGKeyReplace: "Replace the secret of one reviewed stored TSIG key",
	api.TSIGKeyReseal:  "Re-encrypt one reviewed stored TSIG key under the current encryption key",
	api.TSIGKeyRevoke:  "Revoke one reviewed stored TSIG key",
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// readTSIGSecret reads the base64 secret from a file or stdin. It drops ASCII
// whitespace (so wrapped base64 output works) and requires canonical standard
// base64 of 1 to 512 decoded bytes. Errors never contain the input.
func readTSIGSecret(path string, stdin io.Reader) ([]byte, error) {
	if path == "" {
		return nil, errors.New("--secret-file is required (a file holding the base64 secret, or - for stdin)")
	}
	reader := stdin
	if path != "-" {
		f, err := os.Open(path) //nolint:gosec // the operator names their secret file
		if err != nil {
			return nil, errors.New("cannot open the TSIG secret file")
		}
		defer func() { _ = f.Close() }()
		reader = f
	}
	raw, err := io.ReadAll(io.LimitReader(reader, maxTSIGSecretFile+1))
	defer clear(raw)
	if err != nil || len(raw) > maxTSIGSecretFile {
		return nil, errors.New("the TSIG secret file cannot be read or exceeds 4 KiB")
	}
	secret := make([]byte, 0, len(raw))
	for _, b := range raw {
		switch b {
		case ' ', '\t', '\r', '\n':
		default:
			secret = append(secret, b)
		}
	}
	decoded := make([]byte, base64.StdEncoding.DecodedLen(len(secret)))
	defer clear(decoded)
	n, err := base64.StdEncoding.Strict().Decode(decoded, secret)
	if err != nil || n == 0 || n > tsigkeys.MaxSecretBytes || base64.StdEncoding.EncodedLen(n) != len(secret) {
		clear(secret)
		return nil, fmt.Errorf("the TSIG secret must be standard base64 of 1 to %d bytes", tsigkeys.MaxSecretBytes)
	}
	return secret, nil
}

// tsigKeyCommandBody builds the exact request body for one operation. The
// API body types redact themselves when marshaled, so the body is written
// here; secret holds only base64 characters and needs no JSON escaping.
func tsigKeyCommandBody(operation api.TSIGKeyOperation, name, algorithm, uid, rv string, secret []byte) ([]byte, error) {
	var b bytes.Buffer
	field := func(key, value string) {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(key)
		v, _ := json.Marshal(value)
		b.Write(k)
		b.WriteByte(':')
		b.Write(v)
	}
	switch operation {
	case api.TSIGKeyCreate:
		field("keyName", name)
		field("algorithm", algorithm)
	case api.TSIGKeyReplace:
		field("uid", uid)
		field("resourceVersion", rv)
		if algorithm != "" {
			field("algorithm", algorithm)
		}
	case api.TSIGKeyReseal, api.TSIGKeyRevoke:
		field("uid", uid)
		field("resourceVersion", rv)
	default:
		return nil, errors.New("unknown stored TSIG key operation")
	}
	out := make([]byte, 0, b.Len()+len(secret)+16)
	out = append(out, '{')
	out = append(out, b.Bytes()...)
	if secret != nil {
		out = append(out, `,"secret":"`...)
		out = append(out, secret...)
		out = append(out, '"')
	}
	out = append(out, '}')
	return out, nil
}

// tsigKeyCommandError keeps the server's error (status, key, message and
// hint, so scripts can match it) and states that nothing was retried. A
// refusal before the command ran is distinguished from an unknown outcome.
func tsigKeyCommandError(operation api.TSIGKeyOperation, err error) error {
	if errors.Is(err, apiclient.ErrTSIGKeyOutcomeUnknown) {
		return fmt.Errorf("tsigkeys %s: %w", operation, err)
	}
	var apiErr *apiclient.Error
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusServiceUnavailable && apiErr.Key == "primary_required" {
		// An enrolled follower refuses /tsigkeys before the handler runs, so
		// nothing changed. The key stays for the CLI's primary hint.
		return fmt.Errorf("tsigkeys %s: this node is an enrolled follower and did not run the command; nothing changed; run it against the primary: %w", operation, err)
	}
	if errors.As(err, &apiErr) && apiErr.StatusCode < http.StatusInternalServerError {
		return fmt.Errorf("tsigkeys %s refused: %w (not retried; run kudurru tsigkeys list before another attempt)", operation, err)
	}
	return fmt.Errorf("tsigkeys %s: %w (the outcome may be uncertain and was not retried; run kudurru tsigkeys list before any further action)", operation, err)
}
