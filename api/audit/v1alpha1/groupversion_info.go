/*
Copyright 2026 Robert Herbaugh.

SPDX-License-Identifier: LicenseRef-Kudurru-Community
See the LICENSE file in the repository root.
*/

// Package v1alpha1 contains the audit.kudurru.io/v1alpha1 API types: the
// AuditLog batches in which kudurru keeps its audit events in Kubernetes
// when no central database is configured (dev-docs/design-record.md §11.4,
// D157).
//
// The group is separate from kudurru.io on purpose: AuditLogs are runtime
// data, not configuration, so they are not served by the store, not in
// backups (`kind: List` of kudurru.io) and not in `kubectl get kudurru.io`.
//
// +kubebuilder:object:generate=true
// +groupName=audit.kudurru.io
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	// Group is the API group of the AuditLog kind.
	Group = "audit.kudurru.io"
	// Version is the current API version.
	Version = "v1alpha1"
)

var (
	// GroupVersion is the group/version of the audit.kudurru.io API.
	GroupVersion = schema.GroupVersion{Group: Group, Version: Version}

	// SchemeBuilder registers the kinds with a runtime.Scheme.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	// AddToScheme adds the audit.kudurru.io/v1alpha1 kinds to a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
