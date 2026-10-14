/*
Copyright 2026 Robert Herbaugh.

SPDX-License-Identifier: LicenseRef-Kudurru-Community
See the LICENSE file in the repository root.
*/

package v1alpha1

import (
	"encoding/json"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AuditDocument describes a token without its secret hash or arbitrary
// metadata. Use an allowlist so new credential fields cannot enter the log.
func (t *APIToken) AuditDocument() json.RawMessage {
	if t == nil {
		return nil
	}
	doc := struct {
		Spec struct {
			Owner       TokenOwner   `json:"owner"`
			Description string       `json:"description,omitempty"`
			ExpiresAt   *metav1.Time `json:"expiresAt"`
			Scopes      []string     `json:"scopes,omitempty"`
		} `json:"spec"`
	}{}
	doc.Spec.Owner = t.Spec.Owner
	doc.Spec.Description = t.Spec.Description
	doc.Spec.ExpiresAt = t.Spec.ExpiresAt
	doc.Spec.Scopes = t.Spec.Scopes
	b, _ := json.Marshal(doc)
	return b
}
