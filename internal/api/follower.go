package api

import (
	"context"
	"net/http"
	"slices"

	"github.com/danielgtaylor/huma/v2"
)

// ClusterRole is what the API needs to know about the replication role of the
// source a request was admitted on. Only an enrolled SQLite follower sets it.
type ClusterRole struct {
	// Follower is true when the request began on an enrolled follower.
	Follower bool
	// Primary names the member that holds write authority, "" when unknown.
	Primary string
}

// ClusterRoleFunc reports the role of the request's original source. It must
// read only what the request already holds: no store read, no new capture.
type ClusterRoleFunc func(context.Context) ClusterRole

// FollowerClass says what an enrolled SQLite follower does with a mutating
// operation (D368).
type FollowerClass string

// The three classes. Every operation with an unsafe method has exactly one.
const (
	// FollowerForwarded: the follower sends the operation to its primary as a
	// typed member command and relays the primary's answer.
	FollowerForwarded FollowerClass = "forwarded"
	// FollowerLocal: the operation touches only this node's own state, or
	// decides from this node's own role, and runs on the follower.
	FollowerLocal FollowerClass = "local"
	// FollowerPrimary: the follower answers 503 primary_required. It is the
	// class of every operation that the table below does not name.
	FollowerPrimary FollowerClass = "primary"
)

// followerExtension is the OpenAPI extension that carries an operation's
// class, next to x-kudurru-permission.
const followerExtension = "x-kudurru-follower"

// followerOperations classifies every operation with an unsafe method by its
// operationId. An operation that is missing here is FollowerPrimary: a
// follower refuses it. TestFollowerOperationsClassifyEveryMutation fails for
// a registered operation that the table does not name and for a name that is
// no registered operation, so a new route needs a reviewed line here.
//
// Moving an operation to FollowerForwarded needs its typed member command
// (cmd/kudurru/serving_member_command.go) and the follower's request bound
// first; the root package holds the forwarded set to the routes that reach a
// forward.
var followerOperations = map[string]FollowerClass{
	// Canonical object writes (objects.write, objects.delete).
	"apply":                    FollowerForwarded,
	"createObject":             FollowerForwarded,
	"putObject":                FollowerForwarded,
	"putClusterObject":         FollowerForwarded,
	"deleteObject":             FollowerForwarded,
	"deleteClusterObject":      FollowerForwarded,
	"createBlocklists":         FollowerForwarded,
	"putBlocklists":            FollowerForwarded,
	"deleteBlocklists":         FollowerForwarded,
	"createClientgroups":       FollowerForwarded,
	"putClientgroups":          FollowerForwarded,
	"deleteClientgroups":       FollowerForwarded,
	"createPolicies":           FollowerForwarded,
	"putPolicies":              FollowerForwarded,
	"deletePolicies":           FollowerForwarded,
	"batchDeleteDnszones":      FollowerForwarded,
	"batchDeleteDnsrecordsets": FollowerForwarded,
	"batchDeleteBlocklists":    FollowerForwarded,
	"batchDeleteClientgroups":  FollowerForwarded,
	"batchDeletePolicies":      FollowerForwarded,
	// Zone convenience commands (zones.command).
	"upsertZoneRecord": FollowerForwarded,
	"deleteZoneRecord": FollowerForwarded,
	"importZone":       FollowerForwarded,
	"deleteZone":       FollowerForwarded,
	// The blocking switch (blocking.toggle) and local login (auth.login).
	"setBlocking": FollowerForwarded,
	"login":       FollowerForwarded,
	// Sign-out and own-session revocation (auth.logout) and cluster
	// settings (settings.patch).
	"logout":        FollowerForwarded,
	"browserLogout": FollowerForwarded,
	"revokeSession": FollowerForwarded,
	"patchSettings": FollowerForwarded,
	// The caller's own password (auth.password) and personal API tokens
	// (auth.tokens).
	"changePassword": FollowerForwarded,
	"createToken":    FollowerForwarded,
	"deleteToken":    FollowerForwarded,

	// This node's own response cache, query log, resolver and DHCP server.
	"flushCache":            FollowerLocal,
	"purgeQuerylog":         FollowerLocal,
	"verifyRollupEvidence":  FollowerLocal,
	"dnsQuery":              FollowerLocal,
	"putDHCPScopeConfig":    FollowerLocal,
	"putDHCPReservation":    FollowerLocal,
	"deleteDHCPReservation": FollowerLocal,
	"importDHCPLeases":      FollowerLocal,
	"putDHCPSetup":          FollowerLocal,
	// Membership commands decide from this node's own role: promotion runs
	// on a follower; activation and invitations answer 409 on one.
	"promoteCluster":        FollowerLocal,
	"activateCluster":       FollowerLocal,
	"issueClusterJoinToken": FollowerLocal,
	// The legacy settings ownership routes answer the same refusal on every
	// node with canonical settings.
	"changeSettingsOwnership":   FollowerLocal,
	"exportSettingsSectionYAML": FollowerLocal,

	// Allow and deny convenience commands.
	"createAllowlistEntries": FollowerPrimary,
	"patchAllowlistEntry":    FollowerPrimary,
	"deleteAllowlistEntry":   FollowerPrimary,
	"batchAllowlistEntries":  FollowerPrimary,
	"createDenylistEntries":  FollowerPrimary,
	"patchDenylistEntry":     FollowerPrimary,
	"deleteDenylistEntry":    FollowerPrimary,
	"batchDenylistEntries":   FollowerPrimary,
	"addDomains":             FollowerPrimary,
	"queryAction":            FollowerPrimary,
	"patchSourceEntries":     FollowerPrimary,
	// Lists are fetched by the primary; NOTIFY is sent by the primary.
	"refreshBlockList": FollowerPrimary,
	"notifyZone":       FollowerPrimary,
	// DHCPScope objects are not written through a follower.
	"batchDeleteDhcpscopes": FollowerPrimary,
	// A follower cannot store its own log preference, and the primary's
	// would not apply to it.
	"patchNodeConfig": FollowerPrimary,
	// Own authenticators (TOTP enrollment and removal).
	"beginTOTP":   FollowerPrimary,
	"confirmTOTP": FollowerPrimary,
	"disableTOTP": FollowerPrimary,
	// Command-line OIDC login (browser OIDC login is a GET flow and is
	// refused the same way by its request admission).
	"prepareCLILogin": FollowerPrimary,
	"verifyCLILogin":  FollowerPrimary,
	// Identity administration.
	"createLocalUser":        FollowerPrimary,
	"resetLocalUserPassword": FollowerPrimary,
	"recoverTOTP":            FollowerPrimary,
	"disableExternalUser":    FollowerPrimary,
	"enableExternalUser":     FollowerPrimary,
	"logoutExternalUser":     FollowerPrimary,
	"createRoleBinding":      FollowerPrimary,
	"replaceRoleBinding":     FollowerPrimary,
	"deleteRoleBinding":      FollowerPrimary,
	// Stored TSIG keys are sealed by the primary.
	"createStoredTSIGKey":  FollowerPrimary,
	"replaceStoredTSIGKey": FollowerPrimary,
	"resealStoredTSIGKey":  FollowerPrimary,
	"revokeStoredTSIGKey":  FollowerPrimary,
	// Integrations do their provider work under the primary's authority.
	"checkIntegration":        FollowerPrimary,
	"previewIntegration":      FollowerPrimary,
	"syncIntegration":         FollowerPrimary,
	"releaseIntegration":      FollowerPrimary,
	"invokeIntegrationAction": FollowerPrimary,
	// Imports and migrations. The two previews write nothing, but each plans
	// the apply that only the primary runs: the decommission preview's
	// revision is the primary's resourceVersion that the decommission checks,
	// and a follower's replica may lag it. They stay with their apply.
	"previewMigration":             FollowerPrimary,
	"applyMigration":               FollowerPrimary,
	"startMigration":               FollowerPrimary,
	"adoptExistingImports":         FollowerPrimary,
	"decommissionMigration":        FollowerPrimary,
	"previewMigrationDecommission": FollowerPrimary,
	// Unblock request decisions.
	"approveUnblockRequest": FollowerPrimary,
	"denyUnblockRequest":    FollowerPrimary,
	"deleteUnblockRequest":  FollowerPrimary,
	// Operator edge blocks are stored by the primary (D388); every node then
	// serves the replicated set to its edges.
	"createEdgeBlock": FollowerPrimary,
	"revokeEdgeBlock": FollowerPrimary,
}

// followerHints replaces the general hint for an operation whose effect a
// request to the primary would not have.
var followerHints = map[string]string{
	"patchNodeConfig": "a follower cannot store its own log settings, and the primary changes only its own; set log.level and log.format in this node's configuration file or environment",
}

// OperationFollowerClass returns the class of the operation with this
// operationId. Unknown and unnamed operations are FollowerPrimary.
func OperationFollowerClass(operationID string) FollowerClass {
	if class, ok := followerOperations[operationID]; ok {
		return class
	}
	return FollowerPrimary
}

// PrimaryRequired is the one refusal of a mutation that an enrolled follower
// neither forwards nor runs itself (D368). It carries no Retry-After: waiting
// does not change the answer, only the primary or a promotion does. primary
// may be empty when the follower's term carries no name.
func PrimaryRequired(primary string) *Error {
	return primaryRequired(primary, "")
}

// PrimaryRequiredWithHint is PrimaryRequired for a caller that knows a better
// next step than repeating the request on the primary.
func PrimaryRequiredWithHint(primary, hint string) *Error {
	return primaryRequired(primary, hint)
}

func primaryRequired(primary, hint string) *Error {
	message := "this request runs only on the primary; this node is an enrolled follower and does not forward it"
	general := "send it to the API of the primary"
	if primary != "" {
		message = "this request runs only on the primary; this node is an enrolled follower of " + primary + " and does not forward it"
		general += " " + primary
	}
	if hint == "" {
		hint = general + "; GET " + BasePath + "/cluster on this node names the operations a follower forwards or runs itself"
	}
	return newError(http.StatusServiceUnavailable, KeyPrimaryRequired, message, hint)
}

// clusterRole is the request's role, the zero value without a role source.
func (s *Server) clusterRole(ctx context.Context) ClusterRole {
	if s.opts.ClusterRole == nil || ctx == nil {
		return ClusterRole{}
	}
	return s.opts.ClusterRole(ctx)
}

// primaryRequiredMW refuses a FollowerPrimary operation on an enrolled
// follower. Like staticWriteMW it runs after authentication and the route's
// permission check and before the body is read, so a caller without the
// permission still gets 401 or 403 and nothing reaches a handler.
func (s *Server) primaryRequiredMW(ctx huma.Context, next func(huma.Context)) {
	if role := s.clusterRole(ctx.Context()); role.Follower {
		s.writeHumaError(ctx, primaryRequired(role.Primary, followerHints[ctx.Operation().OperationID]))
		return
	}
	next(ctx)
}

// followerRefuses reports whether this request's node refuses the operation
// with primary_required. Capability fields use it so that they say exactly
// what the route would answer.
func (s *Server) followerRefuses(ctx context.Context, operationID string) bool {
	return OperationFollowerClass(operationID) == FollowerPrimary && s.clusterRole(ctx).Follower
}

// followerOperationIDs returns the registered operations of one class, sorted.
func (s *Server) followerOperationIDs(class FollowerClass) []string {
	out := []string{}
	for _, item := range s.api.OpenAPI().Paths {
		for _, op := range []*huma.Operation{item.Post, item.Put, item.Patch, item.Delete} {
			if op != nil && OperationFollowerClass(op.OperationID) == class {
				out = append(out, op.OperationID)
			}
		}
	}
	slices.Sort(out)
	return out
}
