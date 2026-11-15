package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"strings"

	tsigkinds "github.com/robertherbaugh/kudurru/api/tsigkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/tsigkeys"
)

const maxTSIGKeyBody = 4096
const maxTSIGKeyMetadata = 1024

// decodeTSIGKeyChange deliberately never returns JSON parser detail or input.
// Duplicate, unknown, null, nested and operation-inapplicable fields refuse
// before command invocation; raw input is bounded before JSON allocation.
func decodeTSIGKeyChange(operation TSIGKeyOperation, name string, raw []byte) (TSIGKeyChange, error) {
	invalid := func() (TSIGKeyChange, error) {
		return TSIGKeyChange{}, errInvalid("invalid stored TSIG key command body")
	}
	if len(raw) == 0 || len(raw) > maxTSIGKeyBody || len(name) > 254 {
		return invalid()
	}
	allowed := map[string]bool{"uid": true, "resourceVersion": true}
	switch operation {
	case TSIGKeyCreate:
		if name != "" {
			return invalid()
		}
		allowed = map[string]bool{"keyName": true, "algorithm": true, "secret": true}
	case TSIGKeyReplace:
		allowed["algorithm"], allowed["secret"] = true, true
	case TSIGKeyRevoke, TSIGKeyReseal:
	default:
		return invalid()
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return invalid()
	}
	values := make(map[string]string, len(allowed))
	for decoder.More() {
		key, err := decoder.Token()
		field, ok := key.(string)
		if err != nil || !ok || !allowed[field] {
			return invalid()
		}
		if _, duplicate := values[field]; duplicate {
			return invalid()
		}
		token, err := decoder.Token()
		value, ok := token.(string)
		if err != nil || !ok {
			return invalid()
		}
		values[field] = value
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return invalid()
	}
	if decoder.Decode(new(any)) != io.EOF {
		return invalid()
	}
	if operation == TSIGKeyCreate {
		name = values["keyName"]
	}
	if name == "" || len(name) > 254 {
		return invalid()
	}
	name = tsigkinds.CanonicalName(name)
	if name == "" {
		return invalid()
	}
	out := TSIGKeyChange{Operation: operation, KeyName: name, Algorithm: values["algorithm"], UID: values["uid"], ResourceVersion: values["resourceVersion"]}
	if operation != TSIGKeyCreate && (!tsigkinds.ValidID(out.UID) || !tsigkinds.ValidID(out.ResourceVersion)) {
		return invalid()
	}
	if len(out.Algorithm) > 64 {
		return invalid()
	}
	if _, present := values["algorithm"]; present {
		algorithm, ok := dnsserver.TSIGAlgorithm(out.Algorithm)
		if !ok {
			return invalid()
		}
		out.Algorithm = algorithm
	} else if operation == TSIGKeyCreate {
		return invalid()
	}
	if operation == TSIGKeyCreate || operation == TSIGKeyReplace {
		secret := values["secret"]
		if len(secret) == 0 || len(secret) > 684 || strings.ContainsAny(secret, "\r\n") {
			return invalid()
		}
		decoded, err := base64.StdEncoding.Strict().DecodeString(secret)
		if err != nil || len(decoded) == 0 || len(decoded) > tsigkeys.MaxSecretBytes || base64.StdEncoding.EncodeToString(decoded) != secret {
			return invalid()
		}
		out.Secret = decoded
	}
	return out, nil
}
