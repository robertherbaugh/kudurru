package api

import (
	"context"
	"net/http"
	"reflect"
	"strconv"

	"github.com/danielgtaylor/huma/v2"
)

type tsigKeyBodyInput struct {
	RawBody []byte `contentType:"application/json"`
}
type tsigKeyNamedBodyInput struct {
	KeyName string `path:"keyName" maxLength:"254"`
	RawBody []byte `contentType:"application/json"`
}

func (s *Server) registerTSIGKeys() {
	get := operation(http.MethodGet, "/tsigkeys", "listTSIGKeyMetadata", "List captured TSIG key metadata", "tsigkeys")
	addErrorResponses(&get, http.StatusNotImplemented)
	register(s, get, perm{resource: "tsigkeys", verb: "read"}, s.listTSIGKeyMetadata)

	create := s.tsigKeyOperation("/tsigkeys", "createStoredTSIGKey", "Create an absent stored TSIG key", http.StatusCreated)
	register(s, create, perm{resource: "tsigkeys", verb: "write"}, func(ctx context.Context, in *tsigKeyBodyInput) (*tsigKeyCommandOutput, error) {
		return s.commandTSIGKey(ctx, TSIGKeyCreate, "", in.RawBody)
	})
	s.tsigKeyBodySchema(create.Path, CreateStoredTSIGKeyBody{})
	for _, entry := range []struct {
		operation         TSIGKeyOperation
		id, summary, verb string
		schema            any
	}{
		{TSIGKeyReplace, "replaceStoredTSIGKey", "Replace a reviewed stored TSIG key", "write", ReplaceStoredTSIGKeyBody{}},
		{TSIGKeyRevoke, "revokeStoredTSIGKey", "Revoke a reviewed stored TSIG key", "delete", StoredTSIGKeyBinding{}},
		{TSIGKeyReseal, "resealStoredTSIGKey", "Reseal a reviewed stored TSIG key", "write", StoredTSIGKeyBinding{}},
	} {
		op := s.tsigKeyOperation("/tsigkeys/{keyName}/"+string(entry.operation), entry.id, entry.summary, http.StatusOK)
		register(s, op, perm{resource: "tsigkeys", verb: entry.verb}, func(ctx context.Context, in *tsigKeyNamedBodyInput) (*tsigKeyCommandOutput, error) {
			return s.commandTSIGKey(ctx, entry.operation, in.KeyName, in.RawBody)
		})
		s.tsigKeyBodySchema(op.Path, entry.schema)
	}
}

func (s *Server) tsigKeyOperation(path, id, summary string, success int) huma.Operation {
	op := withStatus(operation(http.MethodPost, path, id, summary, "tsigkeys"), success)
	op.SkipValidateBody = true // Strict safe decoder never echoes write-only input.
	op.MaxBodyBytes = maxTSIGKeyBody
	op.Description = "SQLite complete runtime only. One current transaction under original credential, writer and source fences; no mutation replay. A 202 receipt confirms commit without bounded publication acknowledgment. Errors may have an uncertain commit outcome: inspect fresh metadata before further action. File keys are read-only."
	schema := s.api.OpenAPI().Components.Schemas.Schema(reflect.TypeFor[TSIGKeyCommandReceipt](), true, "")
	op.Responses = map[string]*huma.Response{}
	for _, status := range []int{success, http.StatusAccepted} {
		op.Responses[strconv.Itoa(status)] = &huma.Response{Description: http.StatusText(status), Content: map[string]*huma.MediaType{"application/json": {Schema: schema}}}
	}
	addErrorResponses(&op, http.StatusNotImplemented, http.StatusRequestEntityTooLarge)
	return op
}

func (s *Server) tsigKeyBodySchema(path string, body any) {
	doc := documented(s.api.OpenAPI(), http.MethodPost, path)
	if doc != nil {
		schema := s.api.OpenAPI().Components.Schemas.Schema(reflect.TypeOf(body), true, "")
		doc.RequestBody = &huma.RequestBody{Required: true, Content: map[string]*huma.MediaType{"application/json": {Schema: schema}}}
	}
}
