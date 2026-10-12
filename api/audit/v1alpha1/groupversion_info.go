/*
Copyright 2026 The kudurru Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package v1alpha1 contains the audit.kudurru.io/v1alpha1 API types: the
// AuditLog batches in which kudurru keeps its audit events in Kubernetes
// when no central database is configured (docs/architecture.md §11.4,
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
