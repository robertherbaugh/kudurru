package api

import (
	"context"
	"net/http"

	"github.com/robertherbaugh/kudurru/internal/audit"
	"github.com/robertherbaugh/kudurru/internal/config"
)

type ownershipInput struct {
	Section string `path:"section"`
	Action  string `path:"action"`
	Body    struct {
		Revision string `json:"revision"`
	}
}

type settingsYAMLInput struct {
	Section string `path:"section"`
	Body    struct {
		Proposed map[string]any `json:"proposed,omitempty"`
	}
}

type settingsYAMLBody struct {
	YAML     string `json:"yaml"`
	Revision string `json:"revision"`
}
type settingsYAMLOutput struct{ Body *settingsYAMLBody }

func (s *Server) registerSettingsOwnership() {
	register(s, operation(http.MethodPost, "/settings/sections/{section}/ownership/{action}", "changeSettingsOwnership", "Adopt or release a settings section using its reviewed revision", "settings"),
		perm{resource: "settings", verb: "write"}, func(ctx context.Context, in *ownershipInput) (*settingsOutput, error) {
			if s.opts.CanonicalSettings != nil {
				return nil, errUnavailable("canonical settings do not use legacy ownership storage")
			}
			if s.opts.SettingsOwnership == nil {
				return nil, errInvalid("settings ownership requires persistent settings storage")
			}
			if in.Action != "adopt" && in.Action != "release" {
				return nil, errInvalid("action must be adopt or release")
			}
			before := s.opts.SettingsOwnership.Status()
			err := s.opts.SettingsOwnership.Transition(ctx, in.Section, in.Action, in.Body.Revision)
			after := s.opts.SettingsOwnership.Status()
			s.recordAction(ctx, "settings."+in.Action, audit.Resource{Kind: "settings", Name: in.Section}, err, before, after)
			if err != nil {
				return nil, ownershipError(err)
			}
			out := s.settingsBody(s.config(), config.ScopeCluster, nil, nil)
			yes := true
			out.Body.Persisted = &yes
			return out, nil
		})
	register(s, operation(http.MethodPost, "/settings/sections/{section}/yaml", "exportSettingsSectionYAML", "Generate Helm values for the current or proposed complete section without writing", "settings"),
		perm{resource: "settings", verb: "read"}, func(ctx context.Context, in *settingsYAMLInput) (*settingsYAMLOutput, error) {
			if s.opts.CanonicalSettings != nil {
				return nil, errUnavailable("canonical settings do not use legacy ownership storage")
			}
			if s.opts.SettingsOwnership == nil {
				return nil, errInvalid("settings ownership requires persistent settings storage")
			}
			text, revision, err := s.opts.SettingsOwnership.Export(ctx, in.Section, in.Body.Proposed)
			if err != nil {
				return nil, ownershipError(err)
			}
			return &settingsYAMLOutput{Body: &settingsYAMLBody{YAML: text, Revision: revision}}, nil
		})
}
