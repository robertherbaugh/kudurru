package api

import (
	"slices"
	"strconv"
)

// RuntimeShape names one deployment shape of the refusal inventory. The
// operator page docs/deploy/capabilities.md has one column per shape.
type RuntimeShape string

// The shapes the inventory tests mount.
const (
	// ShapeStandalone is a complete SQLite node that is not enrolled.
	ShapeStandalone RuntimeShape = "standalone"
	// ShapePrimary is the enrolled SQLite member that holds write authority.
	ShapePrimary RuntimeShape = "primary"
	// ShapeStatic is a SQLite node with cluster.role static.
	ShapeStatic RuntimeShape = "static"
	// ShapeFollower is an enrolled SQLite follower.
	ShapeFollower RuntimeShape = "follower"
	// ShapeFenced is a former primary that observed a newer one.
	ShapeFenced RuntimeShape = "fenced"
	// ShapeConflicted is a member that recorded conflicting promotions.
	ShapeConflicted RuntimeShape = "conflicted"
	// ShapeKubernetes is a node of the Kubernetes store driver.
	ShapeKubernetes RuntimeShape = "kubernetes"
)

// noRefusal in an operationRefusals line says that a shape answers no
// refusal where it would otherwise inherit one from the standalone shape.
const noRefusal = "-"

// refusals is one operationRefusals line: the key of the 501 or 503 that a
// probe of the operation answers on each shape, with an admin credential, an
// empty JSON object as the body and path parameters that name no object.
//
// A shape that the line does not name inherits: the enrolled primary, the
// static twin and the follower inherit the standalone key (all three run
// the complete SQLite runtime); the follower answers primary_required for a
// FollowerPrimary mutation of followerOperations whatever the line says; a
// fenced or conflicted member answers cluster_fenced or cluster_conflict for
// every operation. The Kubernetes shape inherits nothing.
type refusals map[RuntimeShape]string

// refusalKey explains one 501 or 503 key.
type refusalKey struct {
	// doc is the operator doc anchor that explains the key, relative to
	// the repository root.
	doc string
	// retryAfter says whether the answer carries Retry-After. Only a
	// transient refusal does; a durable one never tells a caller to wait.
	retryAfter retryAfterRule
}

// retryAfterRule says when a refusal carries Retry-After.
type retryAfterRule int

const (
	retryAfterNever retryAfterRule = iota
	retryAfterAlways
	// retryAfterMay: completion_unknown carries one on the authentication
	// routes only (docs/deploy/api-contracts.md), and unavailable names
	// transient states (a provider that is not reachable yet) as well as
	// durable ones.
	retryAfterMay
)

// capabilitiesDoc is the operator page with the capability matrix and the
// glossary of refusal keys.
const capabilitiesDoc = "docs/deploy/capabilities.md"

// refusalKeys is the allowlist of 501 and 503 answers, keyed by
// "<status> <key>". The inventory tests fail for any 501 or 503 whose key
// has no row here, and for one without a hint.
var refusalKeys = map[string]refusalKey{
	"501 " + KeyReadOnly:                {doc: capabilitiesDoc + "#read_only"},
	"501 " + KeyNotImplemented:          {doc: capabilitiesDoc + "#not_implemented"},
	"503 " + KeyReadOnly:                {doc: capabilitiesDoc + "#read_only"},
	"503 " + KeyPrimaryRequired:         {doc: capabilitiesDoc + "#primary_required"},
	"503 " + KeyUnavailable:             {doc: capabilitiesDoc + "#unavailable", retryAfter: retryAfterMay},
	"503 completion_unknown":            {doc: capabilitiesDoc + "#completion_unknown", retryAfter: retryAfterMay},
	"503 committed_output_unavailable":  {doc: capabilitiesDoc + "#committed_output_unavailable"},
	"503 cluster_fenced":                {doc: capabilitiesDoc + "#cluster_fenced"},
	"503 cluster_conflict":              {doc: capabilitiesDoc + "#cluster_conflict"},
	"503 generation_pending":            {doc: capabilitiesDoc + "#generation_pending", retryAfter: retryAfterAlways},
	"503 factor_management_unavailable": {doc: capabilitiesDoc + "#factor_management_unavailable"},
	"503 factor_unavailable":            {doc: capabilitiesDoc + "#factor_unavailable"},
	"503 user_management_unavailable":   {doc: capabilitiesDoc + "#user_management_unavailable"},
	"503 sessions_unavailable":          {doc: capabilitiesDoc + "#sessions_unavailable"},
	"503 external_users_unavailable":    {doc: capabilitiesDoc + "#external_users_unavailable"},
	"503 cli_login_unavailable":         {doc: capabilitiesDoc + "#cli_login_unavailable"},
	"503 provider_logout_unavailable":   {doc: capabilitiesDoc + "#provider_logout_unavailable"},
	"503 integration_uncertain":         {doc: capabilitiesDoc + "#integration_uncertain"},
}

// operationRefusals names every operation of the API, so that a route added
// later fails TestRefusalInventoryNamesEveryOperation until its refusals are
// reviewed. The mounted inventory tests (refusalinventory) hold every line to
// what each runtime shape answers.
var operationRefusals = map[string]refusals{
	// audit
	"exportAudit": {},
	"listAudit":   {},
	"verifyAudit": {},
	// auth
	"authStatus":            {},
	"beginTOTP":             {ShapeStatic: KeyReadOnly, ShapeKubernetes: "factor_management_unavailable"},
	"browserLogout":         {},
	"changePassword":        {ShapeStatic: KeyReadOnly},
	"confirmTOTP":           {ShapeStatic: KeyReadOnly, ShapeKubernetes: "factor_management_unavailable"},
	"createToken":           {ShapeStatic: KeyReadOnly},
	"deleteToken":           {ShapeStatic: KeyReadOnly},
	"disableTOTP":           {ShapeStatic: KeyReadOnly, ShapeKubernetes: "factor_management_unavailable"},
	"getToken":              {},
	"listCLILoginProviders": {ShapeKubernetes: "cli_login_unavailable"},
	"listSessions":          {ShapeKubernetes: "sessions_unavailable"},
	"listTokens":            {},
	"login":                 {},
	"logout":                {},
	"me":                    {},
	"oidcCallback":          {ShapeFollower: KeyPrimaryRequired},
	"oidcLogout":            {ShapeKubernetes: "provider_logout_unavailable"},
	"oidcLogoutCallback":    {ShapeKubernetes: "provider_logout_unavailable"},
	"oidcStart":             {ShapeStandalone: KeyUnavailable, ShapeFollower: KeyPrimaryRequired},
	"prepareCLILogin":       {ShapeKubernetes: "cli_login_unavailable"},
	"revokeSession":         {ShapeKubernetes: "sessions_unavailable"},
	"totpStatus":            {ShapeKubernetes: "factor_management_unavailable"},
	"verifyCLILogin":        {ShapeStandalone: KeyUnavailable, ShapeKubernetes: "cli_login_unavailable"},
	// blocklists
	"addDomains":            {ShapeStatic: KeyReadOnly},
	"batchDeleteBlocklists": {ShapeStatic: KeyReadOnly},
	"createBlocklists":      {ShapeStatic: KeyReadOnly},
	"deleteBlocklists":      {ShapeStatic: KeyReadOnly},
	"getBlocklists":         {},
	"listBlocklists":        {},
	"listSourceEntries":     {},
	"patchSourceEntries":    {ShapeStatic: KeyReadOnly},
	"putBlocklists":         {ShapeStatic: KeyReadOnly},
	"refreshBlockList":      {},
	"searchBlockLists":      {},
	// catalog
	"getCatalogue":            {},
	"getCatalogueIcon":        {},
	"listCatalogueCategories": {},
	"listCatalogueIcons":      {},
	"listCatalogueSafeSearch": {},
	"listCatalogueServices":   {},
	// clientgroups
	"batchDeleteClientgroups": {ShapeStatic: KeyReadOnly},
	"createClientgroups":      {ShapeStatic: KeyReadOnly},
	"deleteClientgroups":      {ShapeStatic: KeyReadOnly},
	"getClientgroups":         {},
	"listClientgroups":        {},
	"putClientgroups":         {ShapeStatic: KeyReadOnly},
	// cluster
	"activateCluster":       {ShapeStatic: KeyReadOnly, ShapeKubernetes: KeyUnavailable},
	"doctor":                {},
	"getCluster":            {},
	"getClusterEnrollment":  {ShapeStatic: KeyUnavailable, ShapeKubernetes: KeyUnavailable},
	"getClusterPromotion":   {ShapeStandalone: KeyUnavailable, ShapePrimary: noRefusal, ShapeFollower: noRefusal, ShapeKubernetes: KeyUnavailable},
	"getEdge":               {},
	"getLicense":            {},
	"infoMessages":          {},
	"issueClusterJoinToken": {ShapeStatic: KeyUnavailable, ShapeKubernetes: KeyUnavailable},
	"listNodes":             {},
	"nodeHealth":            {},
	"promoteCluster":        {ShapeStandalone: KeyUnavailable, ShapePrimary: noRefusal, ShapeFollower: noRefusal, ShapeKubernetes: KeyUnavailable},
	"vipEventStream":        {},
	// dhcp
	"deleteDHCPReservation": {ShapeStatic: KeyReadOnly},
	"exportDHCPLeases":      {},
	"getDHCPScopeConfig":    {},
	"getDHCPSetup":          {},
	"getDHCPStatus":         {},
	"importDHCPLeases":      {ShapeStatic: KeyReadOnly},
	"listDHCPInterfaces":    {},
	"listDHCPLeases":        {},
	"listDHCPReservations":  {},
	"putDHCPReservation":    {ShapeStatic: KeyReadOnly},
	"putDHCPScopeConfig":    {ShapeStatic: KeyReadOnly},
	"putDHCPSetup":          {ShapeStatic: KeyReadOnly},
	// dhcpscopes
	"batchDeleteDhcpscopes": {ShapeStatic: KeyReadOnly},
	// dnsrecordsets
	"batchDeleteDnsrecordsets": {ShapeStatic: KeyReadOnly},
	// dnszones
	"batchDeleteDnszones": {ShapeStatic: KeyReadOnly},
	// domains
	"batchAllowlistEntries":  {ShapeStatic: KeyReadOnly},
	"batchDenylistEntries":   {ShapeStatic: KeyReadOnly},
	"createAllowlistEntries": {ShapeStatic: KeyReadOnly},
	"createDenylistEntries":  {ShapeStatic: KeyReadOnly},
	"deleteAllowlistEntry":   {ShapeStatic: KeyReadOnly},
	"deleteDenylistEntry":    {ShapeStatic: KeyReadOnly},
	"explainAllowRegex":      {},
	"explainDenyRegex":       {},
	"getAllowlistEntry":      {},
	"getDenylistEntry":       {},
	"listAllowlist":          {},
	"listDenylist":           {},
	"listDomainEntries":      {},
	"patchAllowlistEntry":    {ShapeStatic: KeyReadOnly},
	"patchDenylistEntry":     {ShapeStatic: KeyReadOnly},
	// iam
	"createLocalUser":        {ShapeStatic: KeyReadOnly, ShapeKubernetes: "user_management_unavailable"},
	"createRoleBinding":      {ShapeStatic: KeyReadOnly, ShapeKubernetes: KeyUnavailable},
	"deleteRoleBinding":      {ShapeStatic: KeyReadOnly, ShapeKubernetes: KeyUnavailable},
	"disableExternalUser":    {ShapeStatic: KeyReadOnly, ShapeKubernetes: "external_users_unavailable"},
	"enableExternalUser":     {ShapeStatic: KeyReadOnly, ShapeKubernetes: "external_users_unavailable"},
	"getExternalUser":        {ShapeKubernetes: "external_users_unavailable"},
	"getRoleBinding":         {},
	"listExternalUsers":      {ShapeKubernetes: "external_users_unavailable"},
	"listIdentityProviders":  {},
	"listRoleBindings":       {},
	"listServiceAccounts":    {},
	"listUsers":              {},
	"logoutExternalUser":     {ShapeStatic: KeyReadOnly, ShapeKubernetes: "external_users_unavailable"},
	"recoverTOTP":            {ShapeStatic: KeyReadOnly, ShapeKubernetes: "factor_management_unavailable"},
	"replaceRoleBinding":     {ShapeStatic: KeyReadOnly, ShapeKubernetes: KeyUnavailable},
	"resetLocalUserPassword": {ShapeStatic: KeyReadOnly, ShapeKubernetes: "user_management_unavailable"},
	// imports
	"adoptExistingImports":         {ShapeStatic: KeyReadOnly},
	"applyMigration":               {ShapeStatic: KeyReadOnly},
	"decommissionMigration":        {ShapeStatic: KeyReadOnly},
	"getMigrationRun":              {},
	"listExistingImports":          {},
	"listMigrationRuns":            {},
	"previewMigration":             {ShapeStatic: KeyReadOnly},
	"previewMigrationDecommission": {ShapeStatic: KeyReadOnly},
	"startMigration":               {ShapeStatic: KeyReadOnly},
	// integrations
	"checkIntegration":        {},
	"getIntegration":          {ShapeKubernetes: KeyUnavailable},
	"invokeIntegrationAction": {ShapeStatic: KeyReadOnly},
	"listIntegrationActions":  {},
	"listIntegrations":        {},
	"previewIntegration":      {},
	"releaseIntegration":      {ShapeStatic: KeyReadOnly},
	"syncIntegration":         {ShapeStatic: KeyReadOnly},
	// meta
	"getBranding": {},
	"getVersion":  {},
	// objects
	"apply":               {ShapeStatic: KeyReadOnly},
	"createObject":        {ShapeStatic: KeyReadOnly},
	"deleteClusterObject": {ShapeStatic: KeyReadOnly},
	"deleteObject":        {ShapeStatic: KeyReadOnly},
	"getClusterObject":    {},
	"getObject":           {},
	"listObjects":         {},
	"putClusterObject":    {ShapeStatic: KeyReadOnly},
	"putObject":           {ShapeStatic: KeyReadOnly},
	// policies
	"batchDeletePolicies": {ShapeStatic: KeyReadOnly},
	"createPolicies":      {ShapeStatic: KeyReadOnly},
	"deletePolicies":      {ShapeStatic: KeyReadOnly},
	"getPolicies":         {},
	"getPolicyState":      {},
	"listPolicies":        {},
	"putPolicies":         {ShapeStatic: KeyReadOnly},
	// querylog
	"exportQueries":        {},
	"listQueries":          {},
	"purgeQuerylog":        {},
	"queryAction":          {ShapeStatic: KeyReadOnly},
	"queryStream":          {},
	"querySuggestions":     {},
	"querylogStatus":       {},
	"rollupEvidence":       {},
	"verifyRollupEvidence": {},
	// runtime
	"dnsQuery":     {},
	"flushCache":   {},
	"getBlocking":  {},
	"inspectCache": {},
	"setBlocking":  {},
	// settings
	"changeSettingsOwnership":   {ShapeStandalone: KeyUnavailable, ShapeStatic: KeyReadOnly},
	"exportSettingsSectionYAML": {ShapeStandalone: KeyUnavailable},
	"getNodeConfig":             {},
	"getSettings":               {},
	"patchNodeConfig":           {ShapeKubernetes: KeyReadOnly},
	"patchSettings":             {ShapeStatic: KeyReadOnly},
	"previewBlockPage":          {},
	"settingsSchema":            {},
	// stats
	"listClients":           {},
	"statsClientTimeseries": {},
	"statsRecentBlocked":    {},
	"statsSummary":          {},
	"statsTimeseries":       {},
	"statsTop":              {},
	"statsUpstreams":        {},
	// tsigkeys
	"createStoredTSIGKey":  {ShapeStatic: KeyReadOnly},
	"listTSIGKeyMetadata":  {},
	"replaceStoredTSIGKey": {ShapeStatic: KeyReadOnly},
	"resealStoredTSIGKey":  {ShapeStatic: KeyReadOnly},
	"revokeStoredTSIGKey":  {ShapeStatic: KeyReadOnly},
	// unblockrequests
	"approveUnblockRequest": {ShapeStatic: KeyReadOnly},
	"deleteUnblockRequest":  {ShapeStatic: KeyReadOnly},
	"denyUnblockRequest":    {ShapeStatic: KeyReadOnly},
	"getUnblockRequest":     {},
	"listUnblockRequests":   {},
	// zones
	"deleteZone":       {ShapeStatic: KeyReadOnly},
	"deleteZoneRecord": {ShapeStatic: KeyReadOnly},
	"exportZone":       {},
	"getZone":          {},
	"importZone":       {ShapeStatic: KeyReadOnly},
	"listZoneRecords":  {},
	"listZones":        {},
	"notifyZone":       {},
	"upsertZoneRecord": {ShapeStatic: KeyReadOnly},
}

// Feature names a part of a node's configuration that the operator may turn
// off. A node without it answers 503 unavailable, with a hint that names the
// setting, for the operations of featureRefusals, on every shape.
type Feature string

// The features whose absence the inventory walks see.
const (
	// FeatureQueryLog is the SQLite query log (querylog.enabled and
	// querylog.sink sqlite).
	FeatureQueryLog Feature = "querylog"
	// FeatureDHCP is the node's DHCPv4 service (dhcp.enabled).
	FeatureDHCP Feature = "dhcp"
	// FeatureVIP is local VRRP virtual address observation.
	FeatureVIP Feature = "vip"
)

// featureRefusals names the operations that refuse on a node where their
// feature is off. They are not shape limits: the operator turns them on.
var featureRefusals = map[string]Feature{
	"listClients":           FeatureQueryLog,
	"listQueries":           FeatureQueryLog,
	"exportQueries":         FeatureQueryLog,
	"querylogStatus":        FeatureQueryLog,
	"statsClientTimeseries": FeatureQueryLog,
	"statsRecentBlocked":    FeatureQueryLog,
	"statsSummary":          FeatureQueryLog,
	"statsTimeseries":       FeatureQueryLog,
	"getDHCPScopeConfig":    FeatureDHCP,
	"exportDHCPLeases":      FeatureDHCP,
	"listDHCPLeases":        FeatureDHCP,
	"listDHCPReservations":  FeatureDHCP,
	"vipEventStream":        FeatureVIP,
}

// sessionEndingOperations end the session that a probe used.
var sessionEndingOperations = []string{"logout", "browserLogout"}

// SessionEndingOperations returns the operations that a refusal walk must
// probe last because they may end its session.
func SessionEndingOperations() []string { return append([]string(nil), sessionEndingOperations...) }

// ExpectedRefusal returns the 501/503 key that a probe of the operation
// answers on the shape, "" for none, on a node where the features in off are
// turned off. declared is false when the inventory does not name the
// operation at all.
func ExpectedRefusal(shape RuntimeShape, operationID, method string, off ...Feature) (key string, declared bool) {
	line, declared := operationRefusals[operationID]
	if !declared {
		return "", false
	}
	switch shape {
	case ShapeFenced:
		return "cluster_fenced", true
	case ShapeConflicted:
		return "cluster_conflict", true
	case ShapeFollower:
		if unsafeMethod(method) && OperationFollowerClass(operationID) == FollowerPrimary {
			return KeyPrimaryRequired, true
		}
	}
	key, named := line[shape]
	if !named && shape != ShapeKubernetes {
		key = line[ShapeStandalone]
	}
	if key == noRefusal {
		key = ""
	}
	if feature, ok := featureRefusals[operationID]; key == "" && ok && slices.Contains(off, feature) {
		key = KeyUnavailable
	}
	return key, true
}

// RefusalKeyDoc returns the operator doc anchor that explains a 501 or 503
// key, and whether the allowlist has the key with that status.
func RefusalKeyDoc(key string, status int) (string, bool) {
	row, ok := refusalKeys[strconv.Itoa(status)+" "+key]
	return row.doc, ok
}

// RefusalRetryAfterAllowed reports whether a 501 or 503 with this key and
// status may carry Retry-After (allowed) and whether it must (required).
func RefusalRetryAfterAllowed(key string, status int) (allowed, required bool) {
	row := refusalKeys[strconv.Itoa(status)+" "+key]
	return row.retryAfter != retryAfterNever, row.retryAfter == retryAfterAlways
}
