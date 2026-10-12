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

// Package v1alpha1 contains the kudurru.io/v1alpha1 API types.
//
// These structs are the single object model of kudurru: they are the
// Kubernetes CRDs, what the SQLite store driver persists as JSON, and what
// the REST API, CLI and UI exchange (docs/architecture.md §3.4, §4.1).
//
// +kubebuilder:object:generate=true
// +groupName=kudurru.io
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	// Group is the API group of every kudurru kind.
	Group = "kudurru.io"
	// Version is the current API version.
	Version = "v1alpha1"

	// ManagedByAnnotation records the single writer of an object:
	// "api", "file:<path>", "integration:<name>", "bootstrap" or "system".
	// See docs/architecture.md §4.3 (Apply contract) and decision D9.
	ManagedByAnnotation = "kudurru.io/managed-by"
)

var (
	// GroupVersion is the group/version of the kudurru.io API.
	GroupVersion = schema.GroupVersion{Group: Group, Version: Version}

	// SchemeBuilder registers the kinds with a runtime.Scheme.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	// AddToScheme adds the kudurru.io/v1alpha1 kinds to a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion,
		&DNSZone{}, &DNSZoneList{},
		&DNSRecordSet{}, &DNSRecordSetList{},
		&BlockList{}, &BlockListList{},
		&ClientGroup{}, &ClientGroupList{},
		&Policy{}, &PolicyList{},
		&UnblockRequest{}, &UnblockRequestList{},
	)
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}

// Resource returns the GroupResource for a resource name.
func Resource(resource string) schema.GroupResource {
	return GroupVersion.WithResource(resource).GroupResource()
}
