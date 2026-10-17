/*
Copyright 2026 Robert Herbaugh.

SPDX-License-Identifier: LicenseRef-Kudurru-Community
See the LICENSE file in the repository root.
*/

// Package v1alpha1 contains the dhcp.kudurru.io/v1alpha1 API types: the
// DHCPLease objects that hold DHCPv4 lease state when kudurru runs on the
// Kubernetes store (docs/architecture/dhcp.md, D180).
//
// The group is separate from kudurru.io on purpose, like audit.kudurru.io
// (D157): leases are runtime data, not configuration, so they are not
// served by the store, not in backups (`kind: List` of kudurru.io), not in
// the DNS snapshot and not in `kubectl get kudurru.io`.
//
// +kubebuilder:object:generate=true
// +groupName=dhcp.kudurru.io
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	// Group is the API group of the DHCPLease kind.
	Group = "dhcp.kudurru.io"
	// Version is the current API version.
	Version = "v1alpha1"
)

var (
	// GroupVersion is the group/version of the dhcp.kudurru.io API.
	GroupVersion = schema.GroupVersion{Group: Group, Version: Version}

	// SchemeBuilder registers the kinds with a runtime.Scheme.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	// AddToScheme adds the dhcp.kudurru.io/v1alpha1 kinds to a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
