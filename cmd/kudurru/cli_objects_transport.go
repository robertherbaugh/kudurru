package main

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/robertherbaugh/kudurru/internal/apiclient"
	"github.com/robertherbaugh/kudurru/internal/cli"
	"github.com/spf13/cobra"
)

type objectClient interface {
	GetObject(context.Context, string, string, string) (json.RawMessage, error)
	ListObjects(context.Context, string, apiclient.ListOptions) ([]json.RawMessage, error)
	Apply(context.Context, []json.RawMessage) ([]apiclient.ApplyResult, error)
	DeleteObject(context.Context, string, string, string) error
}

func (o *cliOpts) transportKind() string {
	if o.transport == "" {
		return "api"
	}
	return o.transport
}

func (o *cliOpts) apiTransport() error {
	if o.transportKind() != "api" {
		return errors.New("this command requires --transport api; Kubernetes transport supports get, apply, delete and describe only")
	}
	if o.kubeconfig != "" {
		return errors.New("--kubeconfig requires explicit --transport kubernetes")
	}
	return nil
}

func (o *cliOpts) objectClient(cmd *cobra.Command) (objectClient, cli.Resolved, func(), error) {
	if o.transportKind() == "api" {
		c, r, err := o.client()
		return c, r, func() {}, err
	}
	if o.transportKind() != "kubernetes" {
		return nil, cli.Resolved{}, nil, errors.New("--transport must be api or kubernetes")
	}
	// Never silently send API credentials to a kubeconfig-selected server or
	// reinterpret the stored HTTP contexts file as Kubernetes authority.
	if o.server != "" || o.token != "" || o.caFile != "" || o.insecure || o.cliConfig != "" || o.getenv(cli.EnvServer) != "" || o.getenv(cli.EnvToken) != "" {
		return nil, cli.Resolved{}, nil, errors.New("kubernetes transport cannot combine API server/token/CA/insecure or --cli-config overrides; configure credentials in kubeconfig")
	}
	if err := cmd.Context().Err(); err != nil {
		return nil, cli.Resolved{}, nil, err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), o.timeout)
	selected := o.context
	if selected == "" {
		selected = o.getenv(cli.EnvContext)
	}
	c, ns, err := cli.NewKubeObjects(cli.KubeOptions{ConfigPath: o.kubeconfig, Context: selected, Namespace: o.namespace, Timeout: o.timeout, Getenv: o.getenv})
	if err != nil {
		cancel()
		return nil, cli.Resolved{}, nil, err
	}
	cmd.SetContext(ctx)
	return c, cli.Resolved{Namespace: ns}, func() { cancel(); _ = c.Close() }, nil
}

func objectDiagnoses(ctx context.Context, c objectClient) ([]diagnosis, error) {
	if apiClient, ok := c.(*apiclient.Client); ok {
		return diagnoses(ctx, apiClient)
	}
	return nil, nil
}

func kubeTransportHint(err error) string {
	var direct *cli.KubernetesError
	if !errors.As(err, &direct) {
		return ""
	}
	return "check the selected kubeconfig context, credentials and Kubernetes RBAC"
}
