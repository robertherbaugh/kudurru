package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/iammigrate"
)

type iamMigrationBackend interface {
	Check(context.Context) (iammigrate.Status, error)
	Plan(context.Context) (*iammigrate.Plan, error)
	Begin(context.Context, *iammigrate.Plan, iammigrate.OfflineAcknowledgement) (iammigrate.Status, error)
	Resume(context.Context, string, iammigrate.OfflineAcknowledgement) (iammigrate.Status, error)
}

type iamMigrationOptions struct {
	Namespace, Kubeconfig, Context string
	Timeout                        time.Duration
}

type iamMigrationFactory func(*cobra.Command, iamMigrationOptions) (iamMigrationBackend, error)

func newIAMCmd(configPath *string) *cobra.Command {
	cmd := &cobra.Command{Use: "iam", Short: "Manage identity configuration and upgrades"}
	cmd.AddCommand(newIAMMigrationCmd(func(cmd *cobra.Command, o iamMigrationOptions) (iamMigrationBackend, error) {
		cfg, err := loadConfig(cmd, *configPath)
		if err != nil {
			return nil, err
		}
		if cfg.Store.Driver != "kube" {
			return nil, errors.New("IAM migration requires a server configuration with store.driver: kube")
		}
		loading := clientcmd.NewDefaultClientConfigLoadingRules()
		loading.ExplicitPath = o.Kubeconfig
		clientConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loading, &clientcmd.ConfigOverrides{CurrentContext: o.Context})
		rc, err := clientConfig.ClientConfig()
		if err != nil {
			return nil, errors.New("could not load Kubernetes client configuration")
		}
		return newKubeIAMMigrator(rc, cfg, o.Namespace)
	}))
	return cmd
}

// Construct uncached clients without opening the runtime store, starting its
// watches or bootstrapping accounts. Migration must work while servers are down.
func newKubeIAMMigrator(rc *rest.Config, cfg *config.Config, namespace string) (*iammigrate.Migrator, error) {
	core, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return nil, errors.New("could not construct Kubernetes IAM client")
	}
	dc, err := dynamic.NewForConfig(rc)
	if err != nil {
		return nil, errors.New("could not construct Kubernetes identity configuration client")
	}
	return iammigrate.New(core, dc, iammigrate.Options{Namespace: namespace, AllowedFiles: cfg.Credentials.AllowedFiles})
}

func newIAMMigrationCmd(open iamMigrationFactory) *cobra.Command {
	var opts iamMigrationOptions
	cmd := &cobra.Command{
		Use: "migrate", Short: "Convert legacy Kubernetes IAM Secrets to public objects",
		Long: `Inspect or perform the offline Kubernetes IAM upgrade. Stop every old and
new kudurru process, IAM writer and previous migrator before begin or resume.
Install the new CRDs and back up the release namespace first. The commands use
Kubernetes credentials directly; they do not connect to the kudurru API.

Original Secrets and private migration archives remain rollback artifacts.
Output contains only state, object identifiers and counts, never credentials.
An uncertain Create requires explicit operator recovery; resume never adopts a
public object merely because its annotations or contents match.`,
	}
	cmd.PersistentFlags().StringVar(&opts.Namespace, "release-namespace", "", "required: namespace containing the original private IAM Secrets")
	cmd.PersistentFlags().StringVar(&opts.Kubeconfig, "kubeconfig", "", "Kubernetes client configuration (default standard kubeconfig or in-cluster identity)")
	cmd.PersistentFlags().StringVar(&opts.Context, "kube-context", "", "Kubernetes context (default current kubeconfig context)")
	cmd.PersistentFlags().DurationVar(&opts.Timeout, "timeout", 5*time.Minute, "whole operation deadline (maximum 30m)")
	for _, operation := range []string{"status", "plan", "begin", "resume"} {
		var offline bool
		var rv string
		sub := &cobra.Command{
			Use: operation, Short: map[string]string{
				"status": "Read the admission state and journal resource version",
				"plan":   "Validate the complete migration without writing",
				"begin":  "Capture, archive and migrate while all IAM processes are stopped",
				"resume": "Resume an interrupted migration at an exact journal version",
			}[operation], Args: cobra.NoArgs,
			RunE: func(c *cobra.Command, _ []string) error {
				if opts.Namespace == "" {
					return errors.New("--release-namespace is required; the release namespace is distinct from an object's namespace")
				}
				if opts.Timeout <= 0 || opts.Timeout > 30*time.Minute {
					return errors.New("--timeout must be positive and at most 30m")
				}
				if (operation == "begin" || operation == "resume") && !offline {
					return errors.New("--offline is required: stop all old/new kudurru readers, writers and previous migrators first")
				}
				if operation == "resume" && rv == "" {
					return errors.New("--resource-version is required; use the exact JournalResourceVersion from migrate status")
				}
				ctx, cancel := context.WithTimeout(c.Context(), opts.Timeout)
				defer cancel()
				c.SetContext(ctx)
				m, err := open(c, opts)
				if err != nil {
					return err
				}
				ack := iammigrate.OfflineAcknowledgement{ReadersAndWritersStopped: offline}
				var result any
				switch operation {
				case "status":
					result, err = m.Check(ctx)
				case "plan", "begin":
					var plan *iammigrate.Plan
					plan, err = m.Plan(ctx)
					if err != nil {
						return err
					}
					result = plan.Counts()
					if operation == "begin" {
						result, err = m.Begin(ctx, plan, ack)
					}
				case "resume":
					result, err = m.Resume(ctx, rv, ack)
				}
				// Even failed runs report the latest trusted journal RV for recovery.
				if writeErr := json.NewEncoder(c.OutOrStdout()).Encode(result); writeErr != nil {
					return errors.Join(err, fmt.Errorf("write migration result: %w", writeErr))
				}
				return err
			},
		}
		if operation == "begin" || operation == "resume" {
			sub.Flags().BoolVar(&offline, "offline", false, "acknowledge that all IAM readers/writers and previous migrators are stopped")
		}
		if operation == "resume" {
			sub.Flags().StringVar(&rv, "resource-version", "", "exact journal resource version returned by status (never inferred)")
		}
		cmd.AddCommand(sub)
	}
	return cmd
}
