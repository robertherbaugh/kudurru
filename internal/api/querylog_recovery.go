package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/robertherbaugh/kudurru/internal/querylog/sqlitelog"
)

type rollupEvidenceInput struct {
	HistoryScopeParams
	Source string `query:"source" required:"true" maxLength:"128"`
	Hour   int64  `query:"hour" required:"true" minimum:"1" doc:"Start of a closed UTC hour, Unix seconds."`
}

type rollupVerifyInput struct {
	HistoryScopeParams
	Body sqlitelog.RollupEvidence
}

type rollupEvidenceOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         *sqlitelog.RollupEvidence
}
type rollupVerifyBody struct {
	Valid bool `json:"valid"`
}
type rollupVerifyOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         rollupVerifyBody
}

func (s *Server) registerRollupRecovery() {
	permission := perm{resource: "querylog", verb: "read"}
	register(s, operation(http.MethodGet, "/querylog/rollup-evidence", "rollupEvidence", "Capture bounded retained-input recovery evidence", "querylog"), permission, s.rollupEvidence)
	register(s, operation(http.MethodPost, "/querylog/rollup-evidence/verify", "verifyRollupEvidence", "Verify recovery evidence without changing history", "querylog"), permission, s.verifyRollupEvidence)
}

func (s *Server) recoveryReader(scope, node string) (*sqlitelog.Reader, error) {
	if scope != "" && scope != "node" && scope != "cluster" {
		return nil, errInvalid("scope: expected node or cluster")
	}
	if scope == "cluster" && node != "" {
		return nil, errInvalid("node is only valid with scope=node")
	}
	if scope != "cluster" && node == "" {
		node = s.opts.NodeName
	}
	r := s.opts.CentralQuerylog
	if r == nil {
		return nil, errUnavailable("central rollup recovery unavailable")
	}
	if node != "" {
		r = r.ForNode(node)
		if r == nil {
			return nil, errUnavailable("central rollup recovery unavailable")
		}
	}
	return r, nil
}

func (s *Server) recoveryWindow(hour int64) error {
	if hour <= 0 || hour%3600 != 0 {
		return errInvalid("hour: expected a closed UTC hour boundary")
	}
	_, err := s.window(WindowParams{From: time.Unix(hour, 0).UTC().Format(time.RFC3339), To: time.Unix(hour+3600, 0).UTC().Format(time.RFC3339)})
	return err
}

func recoveryError(err error) error {
	switch {
	case errors.Is(err, sqlitelog.ErrRollupLegacy):
		return newError(http.StatusConflict, KeyConflict, "legacy_provenance_required", "original values and inputs are retained; independently verified historical ownership is required")
	case errors.Is(err, sqlitelog.ErrRollupStale):
		return newError(http.StatusConflict, KeyConflict, "stale_rollup_evidence", "capture current evidence for the same source, node and hour")
	case errors.Is(err, sqlitelog.ErrRollupBound):
		return newError(http.StatusRequestEntityTooLarge, KeyTooLarge, "rollup evidence exceeds its source/hour or input bounds", "")
	case errors.Is(err, sqlitelog.ErrUnknownNode):
		return readerError(err)
	default:
		return errUnavailable("central rollup recovery unavailable")
	}
}

func (s *Server) rollupEvidence(ctx context.Context, in *rollupEvidenceInput) (*rollupEvidenceOutput, error) {
	if err := s.recoveryWindow(in.Hour); err != nil {
		return nil, err
	}
	r, err := s.recoveryReader(in.Scope, in.Node)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	e, err := r.RollupEvidence(ctx, in.Source, in.Hour)
	if err != nil {
		return nil, recoveryError(err)
	}
	if ctx.Err() != nil {
		return nil, recoveryError(ctx.Err())
	}
	return &rollupEvidenceOutput{CacheControl: "no-store", Body: e}, nil
}

func (s *Server) verifyRollupEvidence(ctx context.Context, in *rollupVerifyInput) (*rollupVerifyOutput, error) {
	if err := s.recoveryWindow(in.Body.Hour); err != nil {
		return nil, err
	}
	r, err := s.recoveryReader(in.Scope, in.Node)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := r.VerifyRollup(ctx, in.Body); err != nil {
		return nil, recoveryError(err)
	}
	if ctx.Err() != nil {
		return nil, recoveryError(ctx.Err())
	}
	return &rollupVerifyOutput{CacheControl: "no-store", Body: rollupVerifyBody{Valid: true}}, nil
}
