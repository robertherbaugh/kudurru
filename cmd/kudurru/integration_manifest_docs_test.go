package main

import (
	"os"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/config"
)

func TestKubernetesIntegrationLimitsDocumented(t *testing.T) {
	page, err := os.ReadFile("../../docs/deploy/integrations.md")
	if err != nil {
		t.Fatal(err)
	}
	require := func(value string) {
		t.Helper()
		if value != "" && !strings.Contains(string(page), "`"+value+"`") {
			t.Errorf("Kubernetes integration limit %q is missing from integrations.md", value)
		}
	}
	manifests := integrationManifests("kube", "default", config.CredentialGrants{}, nil, kubeIntegrationLimits)
	for _, item := range manifests.Items {
		require(item.Reason)
		for _, limit := range item.FieldLimits {
			require(limit.Field)
			require(limit.Reason)
		}
	}
	require(reasonNamespaceNotAllowed)
}
