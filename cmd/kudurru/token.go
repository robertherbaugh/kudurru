package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/robertherbaugh/kudurru/internal/auth/tokens"
)

// newTokenCmd is `kudurru token`, offline helpers for API tokens.
func newTokenCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "token", Short: "Offline helpers for kudurru API tokens"}
	var out string
	gen := &cobra.Command{
		Use:   "generate [--out file]",
		Short: "Print a fresh kdr_ token, e.g. for KUDURRU_BOOTSTRAP_TOKEN",
		Long: "Prints a new random token in the kdr_<id>_<secret> wire form without contacting a server. Set it as\n" +
			"KUDURRU_BOOTSTRAP_TOKEN (or write it to the file KUDURRU_BOOTSTRAP_TOKEN_FILE names): the server then\n" +
			"stores its hash as a token of the builtin-admin service account <namespace>/bootstrap. --out writes the\n" +
			"token to a new file with mode 0600 instead of printing it.",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			wire, _ := tokens.Generate()
			if out == "" {
				_, err := fmt.Fprintln(c.OutOrStdout(), wire)
				return err
			}
			f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: the operator names the file
			if err != nil {
				if errors.Is(err, os.ErrExist) {
					return fmt.Errorf("%s exists; refusing to overwrite a token file", out)
				}
				return err
			}
			if _, err := fmt.Fprintln(f, wire); err != nil {
				_ = f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
			_, err = fmt.Fprintf(c.ErrOrStderr(), "wrote a new token to %s (mode 0600)\n", out)
			return err
		},
	}
	gen.Flags().StringVar(&out, "out", "", "write the token to this new file (mode 0600) instead of stdout")
	cmd.AddCommand(gen)
	return cmd
}
