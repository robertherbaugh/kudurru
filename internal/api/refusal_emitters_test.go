package api

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// hintlessRefusals are the 501 and 503 answers of this package that carry no
// hint, keyed "<file> <function>: <message format>" ("<dynamic>" for a
// message that is not a literal), with the number of call sites that answer
// it. Each is an internal fault or a transient state of one request (a
// command, read or receipt that went missing, an invariant a plan broke, a
// bound a read exceeded, a request that ended), whose message is the whole
// explanation: no deployment shape or configuration answers it by design,
// and the refusal inventory walks do not reach it. A deployment limit (a
// shape that does not offer the operation, a feature that is off, a central
// database or peer transport that this node does not have) is never listed
// here: it gets a hint that says what to do instead, and a line in
// operationRefusals or featureRefusals when a walk reaches it.
//
// TestRefusalEmittersExplainThemselves fails for a new hintless 501 or 503
// that this list does not name, for a count that differs from the sites
// (so a second hintless answer that reuses a listed message in the same
// function fails too), and for an entry that no longer exists.
var hintlessRefusals = map[string]int{
	"authz.go authorizeMW: authorizer: %v":                                                                                                                 1,
	"authz.go requireGroup: authorizer: %v":                                                                                                                1,
	"blocking_command.go setBlockingCommand: canonical blocking command result unavailable":                                                                1,
	"blocking_command.go setBlockingCommand: canonical blocking command unavailable":                                                                       1,
	"blocking_command.go setBlockingCommand: original blocking command unavailable":                                                                        1,
	"blockpage.go registerBlockPage: captured block-page preview unavailable":                                                                              2,
	"branding.go registerBranding: captured branding unavailable":                                                                                          2,
	"cache_inspect.go inspectCache: captured cache inspection unavailable":                                                                                 2,
	"catalogue.go catalogueFor: original catalog unavailable":                                                                                              1,
	"dhcp.go dhcpError: DHCP storage: %v":                                                                                                                  1,
	"dhcp.go dhcpScope: DHCP scope: %v":                                                                                                                    1,
	"dhcp.go dhcpStatus: DHCP scope: %v":                                                                                                                   1,
	"dhcp_kube.go kubeApplyScope: DHCP scope write returned %T":                                                                                            1,
	"dhcp_setup.go dhcpListInterfaces: DHCP interface inventory: %v":                                                                                       1,
	"dhcp_setup.go dhcpSetupError: DHCP setup storage: %v":                                                                                                 1,
	"domain_command.go Apply: current domain plan unavailable":                                                                                             1,
	"domain_command.go currentDomainEdits: current domain transaction unavailable":                                                                         1,
	"domain_command.go runDomainCommand: current domain command returned no result":                                                                        1,
	"domain_command.go runDomainCommand: current domain command unavailable":                                                                               1,
	"domain_command.go runDomainCommand: domain command changed non-reference policy fields":                                                               1,
	"domain_command.go runDomainCommand: domain command may only edit existing policy references":                                                          1,
	"domain_command.go runDomainCommand: invalid domain command kind":                                                                                      1,
	"domain_command.go runDomainCommand: missing domain deletion target":                                                                                   1,
	"domain_command_exceptions.go commandCreateExceptions: current domain command unavailable":                                                             1,
	"domain_command_exceptions.go commandQueryAction: current domain command unavailable":                                                                  1,
	"edge_blocks.go EdgeBlockError: the stored edge block set is invalid":                                                                                  1,
	"errors.go storeError: <dynamic>":                                                                                                                      1,
	"exceptions.go sourceRules: source %q: %v":                                                                                                             2,
	"info_message_reads.go add: diagnosis messages exceed bounds or contain invalid fields":                                                                1,
	"info_message_reads.go capturedInfoMessages: diagnosis observation clock unavailable":                                                                  1,
	"info_message_reads.go capturedInfoMessages: operational diagnosis read: %v":                                                                           1,
	"info_message_reads.go capturedInfoMessages: original diagnosis context: %v":                                                                           1,
	"info_message_reads.go capturedInfoMessages: original diagnosis read: %v":                                                                              1,
	"info_message_reads.go capturedInfoMessages: original diagnosis reader unavailable":                                                                    1,
	"integration_objects.go commandIntegrationLicense: current integration license capability unavailable":                                                 1,
	"integration_objects.go commandIntegrationLicense: current integration license expired":                                                                1,
	"integration_objects.go commandIntegrationLicense: current integration license unavailable":                                                            1,
	"integrations.go registerIntegrations: integration manifests unavailable":                                                                              1,
	"lists.go refreshList: refresh: %v":                                                                                                                    1,
	"lists.go searchLists: original compiled lists unavailable":                                                                                            1,
	"migration_command.go Apply: current migration plan unavailable":                                                                                       1,
	"migration_command.go Prepare: current migration program unavailable":                                                                                  1,
	"migration_command.go admitMigration: current migration command unavailable":                                                                           1,
	"migration_current_cutover.go applyCurrentCutover: reviewed cutover has no target nameservers":                                                         1,
	"migration_current_import.go currentImportProgram: current migration settings planner unavailable":                                                     1,
	"migration_current_import.go prepareMigrationDocument: current migration kind %s unavailable":                                                          1,
	"migration_current_import.go startCurrentMigration: Import workers are busy or shutting down; review migration history before starting another import": 1,
	"migration_reads.go migrationReader: migration image mismatch":                                                                                         1,
	"migration_reads.go migrationReader: original migration image unavailable":                                                                             1,
	"migration_reads.go migrationRecordSets: incomplete migration record inventory":                                                                        1,
	"migration_reads.go migrationRecordSets: invalid migration record inventory":                                                                           1,
	"migration_reads.go migrationRecordSets: migration record inventory exceeds bounds":                                                                    1,
	"migration_workers.go startMigration: Import workers are busy or shutting down; review migration history before starting another import":               1,
	"nodes.go infoOperationalMessages: central diagnosis exceeds bounds":                                                                                   1,
	"nodes.go infoOperationalMessages: node health diagnosis exceeds bounds":                                                                               1,
	"nodes.go infoOperationalMessages: object loader diagnosis exceeds bounds":                                                                             1,
	"nodes.go listNodes: membership: %v":                                                                                                                   1,
	"nodes.go nodeHealth: node health: membership unavailable":                                                                                             1,
	"nodes.go nodeHealth: node health: unavailable":                                                                                                        1,
	"nodes.go nodeHealth: peer health: unavailable":                                                                                                        1,
	"object_command_write.go commandPolicyLimit: current policy license capability unavailable":                                                            1,
	"object_command_write.go commandPolicyLimit: current policy license expired":                                                                           1,
	"object_command_write.go commandPolicyLimit: current policy license unavailable":                                                                       1,
	"object_command_write.go commandScan: current %s inventory exceeds command bounds":                                                                     2,
	"object_command_write.go commandScan: current %s inventory is incomplete":                                                                              1,
	"object_command_write.go commandWriteObject: current object command returned no result":                                                                1,
	"object_command_write.go commandZoneDeletable: invalid current record set":                                                                             1,
	"object_command_write.go deleteCommandBody: current object command unavailable":                                                                        1,
	"object_command_write.go writeCommand: current object command unavailable":                                                                             1,
	"object_forward.go ForwardedObjectCommand: current object command returned no result":                                                                  1,
	"object_forward.go ForwardedObjectCommand: object forwarding unavailable":                                                                              1,
	"object_forward.go forwardedWriteResult: primary returned an invalid object":                                                                           1,
	"object_forward.go forwardedWriteResult: primary returned an invalid write result":                                                                     1,
	"object_reads.go objectReadUnavailable: original object generation unavailable":                                                                        1,
	"policies.go policyState: complete policy read capabilities unavailable":                                                                               1,
	"policies.go policyState: original policy list state unavailable":                                                                                      1,
	"policies.go policyState: original policy request ended":                                                                                               1,
	"policies.go policyState: original policy settings unavailable":                                                                                        1,
	"query_export.go exportQueries: query export interrupted":                                                                                              2,
	"query_output.go dnsQueryEmissionMW: the original DNS output generation is unavailable":                                                                1,
	"querylog_recovery.go recoveryError: central rollup recovery unavailable":                                                                              1,
	"querylog_recovery.go recoveryReader: central rollup recovery unavailable":                                                                             1,
	"rpz_feeds.go getBlockListRPZFeeds: this node has no configuration snapshot yet":                                                                       1,
	"runtime.go dnsQuery: the DNS output handoff is unavailable":                                                                                           1,
	"runtime.go dnsQuery: the original DNS output generation is unavailable":                                                                               1,
	"runtime.go dnsQuery: the original DNS serving generation is unavailable":                                                                              2,
	"runtime.go flushCache: current local cache command unavailable":                                                                                       1,
	"runtime.go flushCache: invalid cache flush receipt":                                                                                                   1,
	"runtime.go getBlocking: original blocking observation time unavailable":                                                                               1,
	"runtime.go getBlocking: original blocking state unavailable":                                                                                          2,
	"scoped_querylog.go Queries: too many nodes for cluster query pagination":                                                                              1,
	"scoped_querylog.go queryPeers: membership: %v":                                                                                                        1,
	"scoped_querylog.go scopedReader: central node history unavailable":                                                                                    1,
	"scoped_stream.go scopedQueryStream: too many live query streams":                                                                                      1,
	"settings.go clusterSettings: complete settings image unavailable":                                                                                     1,
	"settings.go patchSettingsExpected: current local node command unavailable":                                                                            1,
	"settings.go patchSettingsExpected: local node command receipt unavailable":                                                                            1,
	"settings.go patchSettingsExpected: primary returned an invalid settings result":                                                                       1,
	"settings_current.go Apply: current settings plan unavailable":                                                                                         1,
	"settings_current.go currentSettings: current settings command unavailable":                                                                            1,
	"settings_forward.go ForwardedSettingsCommand: settings forwarding unavailable":                                                                        1,
	"source_command.go commandPatchSourceEntries: current source command returned no result":                                                               1,
	"source_command.go commandPatchSourceEntries: current source command unavailable":                                                                      1,
	"source_reads.go sourceReadUnavailable: original source body or receipt unavailable":                                                                   1,
	"sourceentries.go scanSourceBody: source %q: %v":                                                                                                       2,
	"stats.go readerError: central query history unavailable":                                                                                              1,
	"stats.go upstreamStates: original upstream routes unavailable":                                                                                        2,
	"tsigkeys_command.go commandTSIGKey: stored TSIG key command binding unavailable; inspect fresh metadata before any further action":                    1,
	"tsigkeys_command.go commandTSIGKey: stored TSIG key command outcome is unavailable; inspect fresh metadata before any further action":                 1,
	"tsigkeys_command.go listTSIGKeyMetadata: TSIG key metadata exceeds response bounds":                                                                   1,
	"tsigkeys_command.go listTSIGKeyMetadata: captured TSIG key metadata unavailable":                                                                      1,
	"tsigkeys_command.go safeTSIGKeyError: TSIG command or read outcome unavailable; inspect fresh metadata before further action":                         1,
	"unblock.go runUnblockCommand: current unblock command returned no result":                                                                             1,
	"unblock.go runUnblockCommand: current unblock command unavailable":                                                                                    1,
	"unblock.go runUnblockCommand: unsupported unblock command":                                                                                            1,
	"vip_events.go vipEventStream: VIP notification source or stream capacity unavailable":                                                                 1,
	"zone_command.go Apply: current zone plan is unavailable":                                                                                              1,
	"zone_command_import.go commandImportZone: current zone command returned no result":                                                                    1,
	"zone_command_import.go commandImportZone: current zone command unavailable":                                                                           1,
	"zone_command_plans.go checkZoneMutation: cascade plan may only delete":                                                                                1,
	"zone_command_plans.go checkZoneMutation: invalid record deletion plan":                                                                                1,
	"zone_command_plans.go checkZoneMutation: invalid record upsert plan":                                                                                  1,
	"zone_command_plans.go checkZoneMutation: invalid zone command action":                                                                                 1,
	"zone_command_plans.go checkZoneMutation: invalid zone command target":                                                                                 1,
	"zone_command_plans.go checkZoneMutation: record deletion plan may only remove endpoints":                                                              1,
	"zone_command_plans.go checkZoneMutation: unsupported zone command kind":                                                                               1,
	"zone_command_plans.go commandDeleteZone: current zone command returned no result":                                                                     1,
	"zone_command_plans.go commandUpsertZoneRecord: current zone command unavailable":                                                                      1,
	"zone_command_plans.go currentZoneObject: current zone transaction unavailable":                                                                        1,
	"zone_command_plans.go currentZoneRecordSets: invalid current record set":                                                                              1,
	"zone_command_plans.go makeZonePlanWithDryRun: duplicate zone command target":                                                                          1,
	"zone_command_plans.go makeZonePlanWithDryRun: missing current deletion target":                                                                        1,
	"zone_command_plans.go makeZonePlanWithDryRun: unsupported zone command kind":                                                                          1,
	"zone_command_plans.go makeZonePlanWithDryRun: zone command exceeds object bounds":                                                                     1,
	"zone_command_plans.go runZoneCommand: current zone command unavailable":                                                                               1,
	"zone_forward.go ForwardedZoneCommand: zone command forwarding unavailable":                                                                            1,
	"zone_forward.go forwardZone: primary returned an invalid zone command result":                                                                         1,
	"zone_notify_command.go notifyZoneCommand: current NOTIFY command result unavailable":                                                                  1,
	"zone_notify_command.go notifyZoneCommand: current NOTIFY command unavailable":                                                                         1,
	"zone_notify_command.go notifyZoneCommand: current NOTIFY target exceeds bounds":                                                                       1,
	"zones.go capturedZoneError: committed zone read: %v":                                                                                                  1,
	"zones.go exportZone: zone contents are unavailable":                                                                                                   1,
	"zones.go exportZone: zone export interrupted":                                                                                                         1,
	"zones.go exportZone: zone export: %v":                                                                                                                 1,
	"zones.go listZoneRecords: zone contents are unavailable":                                                                                              1,
	"zones.go listZoneRecords: zone contents could not be read":                                                                                            1,
	"zones.go notifyZone: notify: %v":                                                                                                                      1,
}

// unresolvedRefusalSites are the newError calls of this package whose status
// or hint the check cannot read from the source (a variable, a call), keyed
// "<file> <function>: status|hint <expression>" with the number of such
// calls. Each is reviewed: the value says why the call answers no hintless
// 501 or 503. TestRefusalEmittersExplainThemselves fails for an unreviewed
// one, so that a hint passed as a variable or a status computed elsewhere
// cannot hide a hintless refusal.
var unresolvedRefusalSites = map[string]struct {
	count  int
	reason string
}{
	"errors.go installHumaErrors: status status": {1, "huma's own errors (validation, negotiation, body limits); their keys come from statusKey and huma answers no 501 or 503 for a handler's refusal"},
	"follower.go primaryRequired: hint hint":     {1, "an empty hint is replaced by the general one before newError"},
}

// TestRefusalEmittersExplainThemselves is the static half of "every 501 and
// 503 has a hint" (G21-11) for refusals that a mounted probe cannot reach:
// it finds every errUnavailable call, and every newError with status 501 or
// 503 and an empty hint, in this package, and counts them per key, so that a
// second hintless answer with a listed message in the same function fails as
// well.
func TestRefusalEmittersExplainThemselves(t *testing.T) {
	found, unresolved := hintlessRefusalSites(t)
	for _, site := range sortedKeys(found) {
		if want := hintlessRefusals[site]; found[site] != want {
			if want == 0 {
				t.Errorf("%q answers a 501 or 503 without a hint (%d times): give it one that says what to do (newError with a hint), or, if it is an internal fault that no deployment answers by design, add it to hintlessRefusals (internal/api/refusal_emitters_test.go)", site, found[site])
			} else {
				t.Errorf("%q answers a 501 or 503 without a hint %d times, and hintlessRefusals lists %d: review each one, then correct the count", site, found[site], want)
			}
		}
	}
	for _, site := range sortedKeys(hintlessRefusals) {
		if found[site] == 0 {
			t.Errorf("hintlessRefusals names %q, which this package no longer answers: remove it", site)
		}
	}
	for _, site := range sortedKeys(unresolved) {
		if reviewed, ok := unresolvedRefusalSites[site]; !ok || reviewed.count != unresolved[site] {
			t.Errorf("%q passes newError a status or hint that this check cannot read (%d times): use a constant, or review it and add it with its count and reason to unresolvedRefusalSites (internal/api/refusal_emitters_test.go)", site, unresolved[site])
		}
	}
	for _, site := range sortedKeys(unresolvedRefusalSites) {
		if unresolved[site] == 0 {
			t.Errorf("unresolvedRefusalSites names %q, which this package no longer has: remove it", site)
		}
	}
	// errNotImplemented always carries its hint.
	if e := errNotImplemented("x"); e.Status != 501 || e.Body.Hint == nil || e.Body.Hint == "" {
		t.Errorf("errNotImplemented answers %d with hint %v", e.Status, e.Body.Hint)
	}
}

// TestRefusalEmitterScanCountsAndResolves holds the scanner itself to what
// the check relies on: repeated sites are counted, a numeric status counts
// as one, constant hints resolve, and a variable hint is reported.
func TestRefusalEmitterScanCountsAndResolves(t *testing.T) {
	const source = `package api

const named = "a named hint"
const empty = ""

func sites(hint string, status int) {
	_ = errUnavailable("same")
	_ = errUnavailable("same")
	_ = errUnavailable("other: %v", 1)
	_ = errUnavailable(dynamic())
	_ = newError(503, KeyUnavailable, "numeric", "")
	_ = newError(http.StatusNotImplemented, KeyNotImplemented, "selector", ` + "``" + `)
	_ = newError(http.StatusServiceUnavailable, KeyUnavailable, "named", named)
	_ = newError(http.StatusServiceUnavailable, KeyUnavailable, "joined", "see " + named)
	_ = newError(http.StatusServiceUnavailable, KeyUnavailable, "empty const", empty)
	_ = newError(http.StatusServiceUnavailable, KeyUnavailable, "variable", hint)
	_ = newError(status, KeyUnavailable, "dynamic status", "")
	_ = newError(http.StatusConflict, KeyConflict, "not a refusal", "")
}
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "fixture.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	found, unresolved := map[string]int{}, map[string]int{}
	scanRefusalFile("fixture.go", file, packageStringConstants([]*ast.File{file}), found, unresolved)
	wantFound := map[string]int{
		"fixture.go sites: same":        2,
		"fixture.go sites: other: %v":   1,
		"fixture.go sites: <dynamic>":   1,
		"fixture.go sites: numeric":     1,
		"fixture.go sites: selector":    1,
		"fixture.go sites: empty const": 1,
	}
	wantUnresolved := map[string]int{"fixture.go sites: hint hint": 1, "fixture.go sites: status status": 1}
	if !maps.Equal(found, wantFound) || !maps.Equal(unresolved, wantUnresolved) {
		t.Fatalf("scan found %v and unresolved %v, want %v and %v", found, unresolved, wantFound, wantUnresolved)
	}
}

// hintlessRefusalSites counts the hintless 501 and 503 sites of the
// package's non-test files, and the newError calls whose status or hint it
// cannot read.
func hintlessRefusalSites(t *testing.T) (found, unresolved map[string]int) {
	t.Helper()
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	var parsed []*ast.File
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		// The names come from the package's own directory listing.
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files[name] = file
		parsed = append(parsed, file)
	}
	constants := packageStringConstants(parsed)
	found, unresolved = map[string]int{}, map[string]int{}
	for _, name := range sortedKeys(files) {
		scanRefusalFile(name, files[name], constants, found, unresolved)
	}
	return found, unresolved
}

// scanRefusalFile adds the hintless 501 and 503 sites of one file to found
// and its unreadable newError arguments to unresolved.
func scanRefusalFile(name string, file *ast.File, constants map[string]ast.Expr, found, unresolved map[string]int) {
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil || fn.Name.Name == "errUnavailable" || fn.Name.Name == "newError" {
			continue
		}
		site := name + " " + fn.Name.Name + ": "
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			ident, ok := call.Fun.(*ast.Ident)
			if !ok {
				return true
			}
			var message ast.Expr
			switch {
			case ident.Name == "errUnavailable" && len(call.Args) > 0:
				message = call.Args[0]
			case ident.Name == "newError" && len(call.Args) == 4:
				refusal, known := refusalStatus(call.Args[0])
				if !known {
					unresolved[site+"status "+exprText(call.Args[0])]++
					return true
				}
				if !refusal {
					return true
				}
				hinted, known := nonEmptyString(call.Args[3], constants, 0)
				if !known {
					unresolved[site+"hint "+exprText(call.Args[3])]++
					return true
				}
				if hinted {
					return true
				}
				message = call.Args[2]
			default:
				return true
			}
			text := "<dynamic>"
			if lit, ok := message.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if unquoted, err := strconv.Unquote(lit.Value); err == nil {
					text = unquoted
				}
			}
			found[site+text]++
			return true
		})
	}
}

// refusalStatus reports whether a status argument is 501 or 503, and whether
// the source says which status it is (a net/http constant or a number).
func refusalStatus(e ast.Expr) (refusal, known bool) {
	switch v := e.(type) {
	case *ast.SelectorExpr:
		if pkg, ok := v.X.(*ast.Ident); ok && pkg.Name == "http" && strings.HasPrefix(v.Sel.Name, "Status") {
			return v.Sel.Name == "StatusServiceUnavailable" || v.Sel.Name == "StatusNotImplemented", true
		}
	case *ast.BasicLit:
		if v.Kind == token.INT {
			return v.Value == "501" || v.Value == "503", true
		}
	}
	return false, false
}

// nonEmptyString reports whether a hint argument is a non-empty string, and
// whether the source says so: a literal, a package string constant, or a
// concatenation with at least one non-empty known part.
func nonEmptyString(e ast.Expr, constants map[string]ast.Expr, depth int) (nonEmpty, known bool) {
	if depth > 8 {
		return false, false
	}
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind != token.STRING {
			return false, false
		}
		text, err := strconv.Unquote(v.Value)
		return text != "", err == nil
	case *ast.Ident:
		if value, ok := constants[v.Name]; ok {
			return nonEmptyString(value, constants, depth+1)
		}
	case *ast.ParenExpr:
		return nonEmptyString(v.X, constants, depth+1)
	case *ast.BinaryExpr:
		if v.Op != token.ADD {
			return false, false
		}
		for _, part := range []ast.Expr{v.X, v.Y} {
			if nonEmpty, known := nonEmptyString(part, constants, depth+1); known && nonEmpty {
				return true, true
			}
		}
		x, xKnown := nonEmptyString(v.X, constants, depth+1)
		y, yKnown := nonEmptyString(v.Y, constants, depth+1)
		return x || y, xKnown && yKnown
	}
	return false, false
}

// packageStringConstants maps each package-level constant to its value
// expression.
func packageStringConstants(files []*ast.File) map[string]ast.Expr {
	out := map[string]ast.Expr{}
	for _, file := range files {
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				value := spec.(*ast.ValueSpec)
				for i, name := range value.Names {
					if i < len(value.Values) {
						out[name.Name] = value.Values[i]
					}
				}
			}
		}
	}
	return out
}

func exprText(e ast.Expr) string {
	var b strings.Builder
	if err := printer.Fprint(&b, token.NewFileSet(), e); err != nil {
		return "<expression>"
	}
	return b.String()
}

func sortedKeys[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}
