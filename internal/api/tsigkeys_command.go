package api

import (
	"context"
	"errors"
	"net/http"
	"slices"

	tsigkinds "github.com/robertherbaugh/kudurru/api/tsigkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
)

type tsigKeyCommandOutput struct {
	Status int
	Body   *TSIGKeyCommandReceipt
}

func validTSIGKeyMetadata(key TSIGKeyMetadata) bool {
	algorithm, ok := dnsserver.TSIGAlgorithm(key.Algorithm)
	if !ok || algorithm != key.Algorithm || key.KeyName == "" || tsigkinds.CanonicalName(key.KeyName) != key.KeyName {
		return false
	}
	switch key.Source {
	case "file":
		return key.UID == "" && key.ResourceVersion == "" && key.CreatedAt == nil
	case "store":
		return tsigkinds.ValidID(key.UID) && tsigkinds.ValidID(key.ResourceVersion) && key.CreatedAt != nil && !key.CreatedAt.IsZero() && key.CreatedAt.Nanosecond() == 0
	default:
		return false
	}
}

func (s *Server) commandTSIGKey(ctx context.Context, operation TSIGKeyOperation, name string, raw []byte) (*tsigKeyCommandOutput, error) {
	if s.opts.TSIGKeyCommand == nil {
		return nil, errNotImplemented("durable stored TSIG key management")
	}
	change, err := decodeTSIGKeyChange(operation, name, raw)
	if err != nil {
		return nil, err
	}
	result, err := s.opts.TSIGKeyCommand(ctx, change)
	if err != nil {
		return nil, safeTSIGKeyError(err)
	}
	// Invalid/absent receipts do not prove the transaction failed or rolled back.
	if result == nil || !result.Committed || result.Operation != operation || result.Removed != (operation == TSIGKeyRevoke) || result.Key.KeyName != change.KeyName || result.Key.Source != "store" || !validTSIGKeyMetadata(result.Key) {
		return nil, errUnavailable("stored TSIG key command outcome is unavailable; inspect fresh metadata before any further action")
	}
	if operation != TSIGKeyCreate && result.Key.UID != change.UID || operation == TSIGKeyRevoke && result.Key.ResourceVersion != change.ResourceVersion {
		return nil, errUnavailable("stored TSIG key command binding unavailable; inspect fresh metadata before any further action")
	}
	detached := *result
	if result.Key.CreatedAt != nil {
		created := result.Key.CreatedAt.UTC()
		detached.Key.CreatedAt = &created
	}
	status := http.StatusAccepted
	if detached.Published {
		status = http.StatusOK
		if operation == TSIGKeyCreate {
			status = http.StatusCreated
		}
	}
	return &tsigKeyCommandOutput{Status: status, Body: &detached}, nil
}

type tsigKeyListOutput struct{ Body TSIGKeyList }

func (s *Server) listTSIGKeyMetadata(ctx context.Context, _ *struct{}) (*tsigKeyListOutput, error) {
	if s.opts.TSIGKeyReads == nil {
		return nil, errNotImplemented("captured TSIG key metadata")
	}
	keys, err := s.opts.TSIGKeyReads(ctx)
	if err != nil {
		return nil, safeTSIGKeyError(err)
	}
	if len(keys) > maxTSIGKeyMetadata {
		return nil, errUnavailable("TSIG key metadata exceeds response bounds")
	}
	result := make([]TSIGKeyMetadata, len(keys))
	seen := make(map[string]bool, len(keys))
	for i, key := range keys {
		if !validTSIGKeyMetadata(key) || seen[key.KeyName] {
			return nil, errUnavailable("captured TSIG key metadata unavailable")
		}
		seen[key.KeyName] = true
		result[i] = key
		if key.CreatedAt != nil {
			created := key.CreatedAt.UTC()
			result[i].CreatedAt = &created
		}
	}
	slices.SortFunc(result, func(a, b TSIGKeyMetadata) int {
		if a.KeyName < b.KeyName {
			return -1
		}
		if a.KeyName > b.KeyName {
			return 1
		}
		return 0
	})
	return &tsigKeyListOutput{Body: TSIGKeyList{Items: result}}, nil
}

// Root supplies safe classified API errors. Unexpected backend errors are never
// serialized: they may contain private carrier/SQL/credential details, and their
// commit outcome cannot be inferred from an error string.
func safeTSIGKeyError(err error) error {
	var classified *Error
	if errors.As(err, &classified) {
		return classified
	}
	return errUnavailable("TSIG command or read outcome unavailable; inspect fresh metadata before further action")
}
