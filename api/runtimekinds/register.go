// Package runtimekinds assembles the server's public and private object registry.
// It declares no new GVKs. Public API admission continues to use api/v1alpha1;
// private runtime objects belong only to storage, replication and full backups.
package runtimekinds

import (
	"slices"

	integration "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	iam "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	zone "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime"
)

// NewScheme registers each public and private type once.
func NewScheme() (*runtime.Scheme, error) {
	scheme, err := iam.NewScheme()
	if err != nil {
		return nil, err
	}
	for _, add := range []func(*runtime.Scheme) error{zone.AddToScheme, integration.AddToScheme} {
		if err := add(scheme); err != nil {
			return nil, err
		}
	}
	return scheme, nil
}

// Private returns a detached sorted carrier manifest. Kubernetes uses Secrets
// for these objects; every driver omits them from safe configuration exports.
func Private() []string {
	out := make([]string, 0, len(iam.Kinds)+len(zone.Kinds)+len(integration.Kinds))
	out = append(out, iam.Kinds...)
	out = append(out, zone.Kinds...)
	out = append(out, integration.Kinds...)
	slices.Sort(out)
	return out
}
