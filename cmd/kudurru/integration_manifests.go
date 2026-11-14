package main

import (
	"context"
	"slices"

	"github.com/robertherbaugh/kudurru/ee/integration"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/license"
)

// integrationDriverLimits is what one driver's host refuses, per provider
// type (D400). It must change with the refusals it describes: the SQLite
// root runs every UniFi push (D401), and the Kubernetes root refuses what
// kubeIntegrationRefusal names (D384).
type integrationDriverLimits struct {
	// unsupported maps a type to the status reason of every instance.
	unsupported map[string]string
	// fields maps a type to the spec fields that make an instance refused,
	// and the ones that have no effect because what they need is refused.
	fields map[string][]api.IntegrationFieldLimit
}

// mirrorInert is the message of a record mirror setting on Kubernetes,
// where the mirror's switch is refused.
const mirrorInert = "Used only by spec.unifi.push.mirrorRecords.enabled, which the Kubernetes store refuses; it has no effect here."

var (
	sqliteIntegrationLimits = integrationDriverLimits{}
	kubeIntegrationLimits   = integrationDriverLimits{
		unsupported: map[string]string{"webhook": reasonUnsupportedOnKubernetes},
		fields: map[string][]api.IntegrationFieldLimit{"unifi": {
			{Field: "spec.unifi.inventory", Effect: "refused", Reason: reasonRecordsUnsupported, Message: kubeRefusalMessage(reasonRecordsUnsupported, "")},
			{Field: "spec.unifi.push.dhcpDNSServers", Effect: "refused", Reason: reasonPushUnsupported, Message: kubeRefusalMessage(reasonPushUnsupported, "")},
			{Field: "spec.unifi.push.mirrorRecords.enabled", Effect: "refused", Reason: reasonPushUnsupported, Message: kubeRefusalMessage(reasonPushUnsupported, "")},
			{Field: "spec.unifi.push.clientActions", Effect: "refused", Reason: reasonPushUnsupported, Message: kubeRefusalMessage(reasonPushUnsupported, "")},
			{Field: "spec.unifi.push.dnsServers", Effect: "inert", Requires: "spec.unifi.push.dhcpDNSServers", Message: "Used only by spec.unifi.push.dhcpDNSServers, which the Kubernetes store refuses; it has no effect here."},
			{Field: "spec.unifi.push.networks", Effect: "inert", Requires: "spec.unifi.push.dhcpDNSServers", Message: "Used only by spec.unifi.push.dhcpDNSServers, which the Kubernetes store refuses; it has no effect here."},
			{Field: "spec.unifi.push.mirrorRecords.types", Effect: "inert", Requires: "spec.unifi.push.mirrorRecords.enabled", Message: mirrorInert},
			{Field: "spec.unifi.push.mirrorRecords.domains", Effect: "inert", Requires: "spec.unifi.push.mirrorRecords.enabled", Message: mirrorInert},
			{Field: "spec.unifi.push.mirrorRecords.labels", Effect: "inert", Requires: "spec.unifi.push.mirrorRecords.enabled", Message: mirrorInert},
			{Field: "spec.unifi.push.mirrorRecords.exclude", Effect: "inert", Requires: "spec.unifi.push.mirrorRecords.enabled", Message: mirrorInert},
		}},
	}
)

// refused is the Refused list of a type: the fields whose limit refuses.
func (l integrationDriverLimits) refused(typ string) []string {
	out := []string{}
	for _, f := range l.fields[typ] {
		if f.Effect == "refused" {
			out = append(out, f.Field)
		}
	}
	return out
}

// integrationManifests builds the manifests answer from the provider types
// compiled into this build and the node's own configuration. It reads no
// store and resolves no credential.
func integrationManifests(driver, namespace string, grants config.CredentialGrants, lic *license.Manager, limits integrationDriverLimits) *api.IntegrationManifests {
	out := &api.IntegrationManifests{Driver: driver, Namespace: namespace, CredentialSources: []string{}, Items: []api.IntegrationManifest{},
		AllowedFiles: slices.Clone(grants.AllowedFiles), AllowedEnvironment: slices.Clone(grants.AllowedEnvironment)}
	out.Licensed = lic != nil && lic.Allows(license.FeatureIntegrations)
	if len(grants.AllowedFiles) > 0 {
		out.CredentialSources = append(out.CredentialSources, "file")
	}
	if len(grants.AllowedEnvironment) > 0 {
		out.CredentialSources = append(out.CredentialSources, "env")
	}
	for _, m := range integration.Types() {
		item := api.IntegrationManifest{Type: m.Type, DisplayName: m.DisplayName, Version: m.Version, Capabilities: []string{}, IOClass: m.IOClass,
			ConfigField: m.Type, ConfigSchema: slices.Clone(m.ConfigSchema), Credentials: []api.IntegrationCredentialKey{}, Docs: m.Docs,
			Supported: true, Refused: limits.refused(m.Type), FieldLimits: slices.Clone(limits.fields[m.Type])}
		if item.FieldLimits == nil {
			item.FieldLimits = []api.IntegrationFieldLimit{}
		}
		for _, c := range m.Capabilities {
			item.Capabilities = append(item.Capabilities, string(c))
		}
		for _, k := range m.SecretKeys {
			item.Credentials = append(item.Credentials, api.IntegrationCredentialKey{Name: k.Path, Required: k.Required, Verify: k.Verify})
		}
		if reason, ok := limits.unsupported[m.Type]; ok {
			item.Supported, item.Reason = false, reason
		}
		out.Items = append(out.Items, item)
	}
	return out
}

// IntegrationManifests implements api.IntegrationAdministration on SQLite.
func (r *integrationRuntime) IntegrationManifests(context.Context) (*api.IntegrationManifests, error) {
	return integrationManifests("sqlite", "", r.s.opts.Config.Credentials, r.s.opts.License, sqliteIntegrationLimits), nil
}

// IntegrationManifests implements api.IntegrationAdministration on
// Kubernetes, where only the release namespace's Integrations run.
func (r *kubeIntegrationRuntime) IntegrationManifests(context.Context) (*api.IntegrationManifests, error) {
	return integrationManifests("kube", r.opts.Namespace, r.opts.Credentials, r.opts.License, kubeIntegrationLimits), nil
}
