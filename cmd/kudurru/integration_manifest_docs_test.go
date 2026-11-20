package main

import (
	"os"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/config"
)

func TestKubernetesIntegrationLimitsDocumented(t *testing.T) {
	readPage := func(path string) string {
		t.Helper()
		page, err := os.ReadFile("../../docs/deploy/" + path) //nolint:gosec // G304: both paths are test constants.
		if err != nil {
			t.Fatal(err)
		}
		return string(page)
	}
	require := func(section, text, value string) {
		t.Helper()
		if value != "" && !strings.Contains(text, "`"+value+"`") {
			t.Errorf("Kubernetes integration limit %q is missing from %s", value, section)
		}
	}
	integrations := readPage("integrations.md")
	overview, _, ok := strings.Cut(integrations, "\n## Deleting an integration")
	if !ok {
		t.Fatal("missing integrations.md overview")
	}
	_, fieldLimits, ok := strings.Cut(integrations, "\n`GET /api/v1/integrations/manifests`")
	if !ok {
		t.Fatal("missing integrations.md manifest limits")
	}
	fieldLimits, _, ok = strings.Cut(fieldLimits, "\n\nA `deletionPolicy: Delete`")
	if !ok {
		t.Fatal("missing end of integrations.md manifest limits")
	}
	_, capabilities, ok := strings.Cut(readPage("capabilities.md"), "\nOn Kubernetes, an Integration's status reports ")
	if !ok {
		t.Fatal("missing capabilities.md Kubernetes Integration status paragraph")
	}
	capabilities, _, _ = strings.Cut(capabilities, "\n\n")
	requireReason := func(reason string) {
		t.Helper()
		require("the integrations.md overview", overview, reason)
		require("the capabilities.md Kubernetes Integration status paragraph", capabilities, reason)
	}

	manifests := integrationManifests("kube", "default", config.CredentialGrants{}, nil, kubeIntegrationLimits)
	for _, item := range manifests.Items {
		requireReason(item.Reason)
		for _, limit := range item.FieldLimits {
			require("the integrations.md manifest limits", fieldLimits, limit.Field)
			requireReason(limit.Reason)
		}
	}
	requireReason(reasonNamespaceNotAllowed)
}
