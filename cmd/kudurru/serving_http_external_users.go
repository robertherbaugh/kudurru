package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/security"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func (h *servingHTTP) registerExternalUsers() {
	h.route(http.MethodGet, "/iam/external-users", h.externalUsers)
	h.route(http.MethodGet, "/iam/external-users/{name}", h.externalUser)
	for _, action := range []string{"disable", "enable", "logout"} {
		h.route(http.MethodPost, "/iam/external-users/{name}/"+action, func(w http.ResponseWriter, r *http.Request) error { return h.externalUserCommand(w, r, action) })
	}
}

func externalUserDTO(info servingauth.ExternalAccountInfo) api.ExternalUser {
	i := info.Identity
	out := api.ExternalUser{Name: info.Name, UID: i.UID, Revision: info.Revision, Provider: i.Provider.ID, ProviderUID: i.Provider.UID, Origin: string(i.Provider.Origin), Issuer: i.Issuer, Subject: i.Subject, Username: i.Username, DisplayName: i.DisplayName, Email: i.Email, Groups: append([]string{}, i.Groups...), Disabled: i.Disabled, CreatedAt: i.CreatedAt}
	if !info.ValidAfter.IsZero() {
		at := info.ValidAfter
		out.ValidAfter = &at
	}
	return out
}

func externalHex(value string, size int) bool {
	if len(value) != size {
		return false
	}
	for _, c := range value {
		if (c < 'a' || c > 'f') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

func (h *servingHTTP) externalUsers(w http.ResponseWriter, r *http.Request) error {
	v, _, err := h.permission(r.Context(), "users", "read")
	if err != nil {
		return err
	}
	if len(r.URL.RawQuery) > 256 {
		return servingHTTPInvalid("invalid account page")
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return servingHTTPInvalid("invalid account page")
	}
	for k, values := range q {
		if (k != "limit" && k != "continue") || len(values) != 1 {
			return servingHTTPInvalid("invalid account page")
		}
	}
	limit := 50
	if q.Has("limit") {
		limit, err = strconv.Atoi(q.Get("limit"))
		if err != nil || limit < 1 || limit > 100 {
			return servingHTTPInvalid("invalid account page limit")
		}
	}
	items, epoch, err := v.ExternalAccounts()
	if err != nil {
		return err
	}
	after := ""
	if q.Has("continue") {
		prefix, name, ok := strings.Cut(q.Get("continue"), ":")
		if !ok || !externalHex(name, 64) {
			return servingHTTPInvalid("invalid account page cursor")
		}
		if prefix != strconv.FormatUint(epoch, 10) {
			return externalUserConflict()
		}
		after = name
	}
	pageBytes := 0
	page := api.AuthList[api.ExternalUser]{Items: []api.ExternalUser{}, TookMS: auth.TookMS(r)}
	for _, item := range items {
		if item.Name <= after {
			continue
		}
		row := externalUserDTO(item)
		encoded, err := json.Marshal(row)
		if err != nil {
			return err
		}
		if len(encoded) > 1<<20 {
			return servingauth.ErrBounds
		}
		if len(page.Items) == limit || pageBytes+len(encoded) > 1<<20 {
			page.Continue = strconv.FormatUint(epoch, 10) + ":" + page.Items[len(page.Items)-1].Name
			break
		}
		page.Items = append(page.Items, row)
		pageBytes += len(encoded)
	}
	if _, _, err := h.permission(r.Context(), "users", "read"); err != nil {
		return err
	}
	return servingHTTPJSON(w, http.StatusOK, page)
}

func (h *servingHTTP) externalUser(w http.ResponseWriter, r *http.Request) error {
	v, _, err := h.permission(r.Context(), "users", "read")
	if err != nil {
		return err
	}
	name := r.PathValue("name")
	if !externalHex(name, 64) || r.URL.RawQuery != "" {
		return servingHTTPInvalid("invalid external account")
	}
	items, _, err := v.ExternalAccounts()
	if err != nil {
		return err
	}
	for _, item := range items {
		if item.Name == name {
			if _, _, err := h.permission(r.Context(), "users", "read"); err != nil {
				return err
			}
			return servingHTTPJSON(w, http.StatusOK, externalUserDTO(item))
		}
	}
	return store.ErrNotFound
}

func externalUserConflict() error {
	return &auth.Error{Status: http.StatusConflict, Key: "conflict", Message: "external account inventory changed; refresh before making a new request"}
}

func (h *servingHTTP) externalUserCommand(w http.ResponseWriter, r *http.Request, action string) error {
	if err := h.localUserPreflight(w, r); err != nil {
		return err
	}
	name := r.PathValue("name")
	if !externalHex(name, 64) || r.URL.RawQuery != "" {
		return servingHTTPInvalid("invalid external account")
	}
	req, err := decodeExternalUserCommand(w, r)
	if err != nil {
		return err
	}
	err = h.localUserCommand(r, name, func(ctx context.Context, tx *sqlite.CommandTx, _ auth.Principal, observed time.Time) error {
		return security.AdministerExternal(ctx, tx, name, req.UID, req.Revision, action, observed)
	})
	if errors.Is(err, store.ErrConflict) {
		return externalUserConflict()
	}
	if err != nil {
		return err
	}
	h.opts.Runtime.publisher.Wake()
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func decodeExternalUserCommand(w http.ResponseWriter, r *http.Request) (api.ExternalUserCommand, error) {
	invalid := func() (api.ExternalUserCommand, error) {
		return api.ExternalUserCommand{}, servingHTTPInvalid("invalid external account command")
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	first, err := d.Token()
	if err != nil || first != json.Delim('{') {
		return invalid()
	}
	values := map[string]string{}
	for d.More() {
		token, err := d.Token()
		key, ok := token.(string)
		if err != nil || !ok || (key != "uid" && key != "revision") {
			return invalid()
		}
		if _, duplicate := values[key]; duplicate {
			return invalid()
		}
		token, err = d.Token()
		value, ok := token.(string)
		if err != nil || !ok {
			return invalid()
		}
		values[key] = value
	}
	last, err := d.Token()
	if err != nil || last != json.Delim('}') {
		return invalid()
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return invalid()
	}
	if !externalHex(values["uid"], 32) || !externalHex(values["revision"], 64) {
		return invalid()
	}
	return api.ExternalUserCommand{UID: values["uid"], Revision: values["revision"]}, nil
}
