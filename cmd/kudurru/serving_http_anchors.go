package main

import (
	"context"
	"net/http"
	"slices"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

// ClusterAnchorsAvailable is the complete nonstatic SQLite runtime, as for
// enrollment review: standalone, primary and follower alike.
func (s *servingRuntime) ClusterAnchorsAvailable() bool { return s.ClusterEnrollmentAvailable() }

// getClusterAnchors is GET /cluster/anchors: a read-only projection of this
// node's installed root-anchor owner, its node-local carrier, its anchor
// workers and its last pull (D369). It reads one read-only transaction and
// memory, inside the original request's read lease; it starts no operation.
func (h *servingHTTP) getClusterAnchors(w http.ResponseWriter, r *http.Request) error {
	s := h.opts.Runtime
	if !s.ClusterAnchorsAvailable() {
		return &auth.Error{Status: http.StatusServiceUnavailable, Key: "unavailable", Message: "root anchor status requires the complete nonstatic SQLite runtime"}
	}
	if r.URL.RawQuery != "" {
		return servingHTTPInvalid("root anchor status does not accept query parameters")
	}
	if _, _, err := h.permission(r.Context(), "cluster", "read"); err != nil {
		return err
	}
	lease, err := servinghttp.CloneLease(r.Context())
	if err != nil {
		return err
	}
	defer lease.Release()
	if err := s.publisher.CheckReadLease(r.Context(), lease); err != nil {
		return err
	}
	status, err := s.rootAnchorStatus(r.Context())
	if err != nil {
		return err
	}
	if err := s.publisher.CheckReadLease(r.Context(), lease); err != nil {
		return err
	}
	return servingHTTPJSON(w, http.StatusOK, status)
}

func (s *servingRuntime) rootAnchorStatus(ctx context.Context) (api.RootAnchorStatus, error) {
	image, local, err := s.opts.Store.ReadAnchorStatus(ctx)
	if err != nil {
		return api.RootAnchorStatus{}, err
	}
	a := s.opts.Store.Authority()
	owner, carrier, authority, err := rootAnchorProjection(image, local, a)
	if err != nil {
		return api.RootAnchorStatus{}, err
	}
	host := s.opts.Config.Upstreams.Recursion
	status := api.RootAnchorStatus{Node: s.opts.Config.Node.Name, AnchorMode: string(host.AnchorMode), ManagedTopology: host.ManagedTopology,
		Authority: authority, Owner: owner, Local: carrier, AdmissionCurrent: s.anchorAdmissionCurrent()}
	if mode, ok := rootAnchorEffectiveMode(s.opts.Config, image); ok {
		status.AnchorMode = mode
	}
	snap := s.anchorStatus.snapshot()
	status.Updater, status.Supervisor = snap.updater.api(), snap.supervisor.api()
	if s.member != nil && a.Mode != replication.Primary && snap.pull.result != "" {
		pull := &api.RootAnchorPull{Result: snap.pull.result, At: snap.pull.at.UTC(), RefusedSince: timePointer(snap.pull.refusedSince)}
		if refusal := snap.pull.refusal; refusal != nil {
			pull.Refusal, pull.RefusedKeyIDs = string(refusal.Reason), slices.Clone(refusal.Keys)
			for _, id := range refusal.Keys {
				for _, key := range owner.Keys {
					if key.ID == id {
						pull.RefusedKeyTags = append(pull.RefusedKeyTags, key.KeyTag)
					}
				}
			}
		}
		status.Pull = pull
	}
	status.Warnings = rootAnchorWarnings(status)
	return status, nil
}

// rootAnchorEffectiveMode is the anchorMode this node applies to the image's
// cluster settings (anchorMode is cluster scope). It is display only.
func rootAnchorEffectiveMode(host *config.Config, image *replication.Image) (string, bool) {
	settings, err := anchorImageSettings(image)
	if err != nil {
		return "", false
	}
	document, err := servingSettingsDocument(settings)
	if err != nil {
		return "", false
	}
	effective, _, err := config.ApplyClusterDocument(host, document)
	if err != nil {
		return "", false
	}
	return string(effective.Upstreams.Recursion.AnchorMode), true
}
