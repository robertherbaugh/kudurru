package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// hintlessRefusals are the 501 and 503 answers of this package that carry no
// hint, keyed "<file> <function>: <message format>". Each is an internal
// fault or a transient state of one request (a command, read or receipt that
// went missing, an invariant a plan broke, a bound a read exceeded, a
// request that ended), whose message is the whole explanation: no
// deployment shape or configuration answers it by design, and the refusal
// inventory walks do not reach it. A deployment limit (a shape that does not
// offer the operation, a feature that is off) is never listed here: it gets
// a hint that says what to do instead, and a line in operationRefusals or
// featureRefusals.
//
// TestRefusalEmittersExplainThemselves fails for a new hintless 501 or 503
// that this list does not name, and for an entry that no longer exists.
var hintlessRefusals = []string{
	"authz.go authorizeMW: authorizer: %v",
	"authz.go require: authorizer: %v",
	"blocking_command.go setBlockingCommand: canonical blocking command result unavailable",
	"blocking_command.go setBlockingCommand: canonical blocking command unavailable",
	"blocking_command.go setBlockingCommand: original blocking command unavailable",
	"blockpage.go registerBlockPage: captured block-page preview unavailable",
	"branding.go registerBranding: captured branding unavailable",
	"cache_inspect.go inspectCache: captured cache inspection unavailable",
	"catalogue.go catalogueFor: original catalog unavailable",
	"dhcp.go dhcpError: DHCP storage: %v",
	"dhcp.go dhcpScope: DHCP scope: %v",
	"dhcp.go dhcpStatus: DHCP scope: %v",
	"dhcp_kube.go kubeApplyScope: DHCP scope write returned %T",
	"dhcp_setup.go dhcpListInterfaces: DHCP interface inventory: %v",
	"dhcp_setup.go dhcpSetupError: DHCP setup storage: %v",
	"domain_command.go Apply: current domain plan unavailable",
	"domain_command.go currentDomainEdits: current domain transaction unavailable",
	"domain_command.go runDomainCommand: current domain command returned no result",
	"domain_command.go runDomainCommand: current domain command unavailable",
	"domain_command.go runDomainCommand: domain command changed non-reference policy fields",
	"domain_command.go runDomainCommand: domain command may only edit existing policy references",
	"domain_command.go runDomainCommand: invalid domain command kind",
	"domain_command.go runDomainCommand: missing domain deletion target",
	"domain_command_exceptions.go commandCreateExceptions: current domain command unavailable",
	"domain_command_exceptions.go commandQueryAction: current domain command unavailable",
	"errors.go storeError: <dynamic>",
	"exceptions.go sourceRules: source %q: %v",
	"info_message_reads.go add: diagnosis messages exceed bounds or contain invalid fields",
	"info_message_reads.go capturedInfoMessages: diagnosis observation clock unavailable",
	"info_message_reads.go capturedInfoMessages: operational diagnosis read: %v",
	"info_message_reads.go capturedInfoMessages: original diagnosis context: %v",
	"info_message_reads.go capturedInfoMessages: original diagnosis read: %v",
	"info_message_reads.go capturedInfoMessages: original diagnosis reader unavailable",
	"lists.go refreshList: refresh: %v",
	"lists.go searchLists: original compiled lists unavailable",
	"migration_command.go Apply: current migration plan unavailable",
	"migration_command.go Prepare: current migration program unavailable",
	"migration_command.go admitMigration: current migration command unavailable",
	"migration_current_cutover.go applyCurrentCutover: reviewed cutover has no target nameservers",
	"migration_current_import.go currentImportProgram: current migration settings planner unavailable",
	"migration_current_import.go prepareMigrationDocument: current migration kind %s unavailable",
	"migration_current_import.go startCurrentMigration: Import workers are busy or shutting down; review migration history before starting another import",
	"migration_reads.go migrationReader: migration image mismatch",
	"migration_reads.go migrationReader: original migration image unavailable",
	"migration_reads.go migrationRecordSets: incomplete migration record inventory",
	"migration_reads.go migrationRecordSets: invalid migration record inventory",
	"migration_reads.go migrationRecordSets: migration record inventory exceeds bounds",
	"migration_workers.go startMigration: Import workers are busy or shutting down; review migration history before starting another import",
	"nodes.go infoOperationalMessages: central diagnosis exceeds bounds",
	"nodes.go infoOperationalMessages: node health diagnosis exceeds bounds",
	"nodes.go infoOperationalMessages: object loader diagnosis exceeds bounds",
	"nodes.go listNodes: membership: %v",
	"nodes.go nodeHealth: %s",
	"nodes.go nodeHealth: node health: membership unavailable",
	"nodes.go nodeHealth: node health: unavailable",
	"nodes.go nodeHealth: peer health: unavailable",
	"object_command_write.go commandPolicyLimit: current policy license capability unavailable",
	"object_command_write.go commandPolicyLimit: current policy license expired",
	"object_command_write.go commandPolicyLimit: current policy license unavailable",
	"object_command_write.go commandScan: current %s inventory exceeds command bounds",
	"object_command_write.go commandScan: current %s inventory is incomplete",
	"object_command_write.go commandWriteObject: current object command returned no result",
	"object_command_write.go commandZoneDeletable: invalid current record set",
	"object_command_write.go deleteCommandBody: current object command unavailable",
	"object_command_write.go writeCommandBody: current object command unavailable",
	"object_forward.go ForwardedObjectCommand: current object command returned no result",
	"object_forward.go ForwardedObjectCommand: object forwarding unavailable",
	"object_forward.go forwardedWriteResult: primary returned an invalid object",
	"object_forward.go forwardedWriteResult: primary returned an invalid write result",
	"object_reads.go objectReadUnavailable: original object generation unavailable",
	"policies.go policyState: complete policy read capabilities unavailable",
	"policies.go policyState: original policy list state unavailable",
	"policies.go policyState: original policy request ended",
	"policies.go policyState: original policy settings unavailable",
	"query_export.go exportQueries: query export interrupted",
	"query_output.go dnsQueryEmissionMW: the original DNS output generation is unavailable",
	"querylog_recovery.go recoveryError: central rollup recovery unavailable",
	"querylog_recovery.go recoveryReader: central rollup recovery unavailable",
	"runtime.go dnsQuery: the DNS output handoff is unavailable",
	"runtime.go dnsQuery: the original DNS output generation is unavailable",
	"runtime.go dnsQuery: the original DNS serving generation is unavailable",
	"runtime.go flushCache: current local cache command unavailable",
	"runtime.go flushCache: invalid cache flush receipt",
	"runtime.go getBlocking: original blocking observation time unavailable",
	"runtime.go getBlocking: original blocking state unavailable",
	"scoped_querylog.go Queries: too many nodes for cluster query pagination",
	"scoped_querylog.go gather: query log unavailable on the requested nodes",
	"scoped_querylog.go queryPeers: membership: %v",
	"scoped_querylog.go scopedReader: central node history unavailable",
	"scoped_stream.go scopedQueryStream: live tail unavailable on the requested nodes",
	"scoped_stream.go scopedQueryStream: too many live query streams",
	"settings.go clusterSettings: complete settings image unavailable",
	"settings.go patchSettingsExpected: current local node command unavailable",
	"settings.go patchSettingsExpected: local node command receipt unavailable",
	"settings.go patchSettingsExpected: primary returned an invalid settings result",
	"settings_current.go Apply: current settings plan unavailable",
	"settings_current.go currentSettings: current settings command unavailable",
	"settings_forward.go ForwardedSettingsCommand: settings forwarding unavailable",
	"source_command.go commandPatchSourceEntries: current source command returned no result",
	"source_command.go commandPatchSourceEntries: current source command unavailable",
	"source_reads.go sourceReadUnavailable: original source body or receipt unavailable",
	"sourceentries.go scanSourceBody: source %q: %v",
	"stats.go readerError: central query history unavailable",
	"stats.go upstreamStates: original upstream routes unavailable",
	"tsigkeys_command.go commandTSIGKey: stored TSIG key command binding unavailable; inspect fresh metadata before any further action",
	"tsigkeys_command.go commandTSIGKey: stored TSIG key command outcome is unavailable; inspect fresh metadata before any further action",
	"tsigkeys_command.go listTSIGKeyMetadata: TSIG key metadata exceeds response bounds",
	"tsigkeys_command.go listTSIGKeyMetadata: captured TSIG key metadata unavailable",
	"tsigkeys_command.go safeTSIGKeyError: TSIG command or read outcome unavailable; inspect fresh metadata before further action",
	"unblock.go runUnblockCommand: current unblock command returned no result",
	"unblock.go runUnblockCommand: current unblock command unavailable",
	"unblock.go runUnblockCommand: unsupported unblock command",
	"vip_events.go vipEventStream: VIP notification source or stream capacity unavailable",
	"zone_command.go Apply: current zone plan is unavailable",
	"zone_command_import.go commandImportZone: current zone command returned no result",
	"zone_command_import.go commandImportZone: current zone command unavailable",
	"zone_command_plans.go checkZoneMutation: cascade plan may only delete",
	"zone_command_plans.go checkZoneMutation: invalid record deletion plan",
	"zone_command_plans.go checkZoneMutation: invalid record upsert plan",
	"zone_command_plans.go checkZoneMutation: invalid zone command action",
	"zone_command_plans.go checkZoneMutation: invalid zone command target",
	"zone_command_plans.go checkZoneMutation: record deletion plan may only remove endpoints",
	"zone_command_plans.go checkZoneMutation: unsupported zone command kind",
	"zone_command_plans.go commandDeleteZone: current zone command returned no result",
	"zone_command_plans.go commandUpsertZoneRecord: current zone command unavailable",
	"zone_command_plans.go currentZoneObject: current zone transaction unavailable",
	"zone_command_plans.go currentZoneRecordSets: invalid current record set",
	"zone_command_plans.go makeZonePlanWithDryRun: duplicate zone command target",
	"zone_command_plans.go makeZonePlanWithDryRun: missing current deletion target",
	"zone_command_plans.go makeZonePlanWithDryRun: unsupported zone command kind",
	"zone_command_plans.go makeZonePlanWithDryRun: zone command exceeds object bounds",
	"zone_command_plans.go runZoneCommand: current zone command unavailable",
	"zone_forward.go ForwardedZoneCommand: zone command forwarding unavailable",
	"zone_forward.go forwardZone: primary returned an invalid zone command result",
	"zone_notify_command.go notifyZoneCommand: current NOTIFY command result unavailable",
	"zone_notify_command.go notifyZoneCommand: current NOTIFY command unavailable",
	"zone_notify_command.go notifyZoneCommand: current NOTIFY target exceeds bounds",
	"zones.go capturedZoneError: committed zone read: %v",
	"zones.go exportZone: zone contents are unavailable",
	"zones.go exportZone: zone export interrupted",
	"zones.go exportZone: zone export: %v",
	"zones.go listZoneRecords: zone contents are unavailable",
	"zones.go listZoneRecords: zone contents could not be read",
	"zones.go notifyZone: notify: %v",
}

// TestRefusalEmittersExplainThemselves is the static half of "every 501 and
// 503 has a hint" (G21-11) for refusals that a mounted probe cannot reach:
// it finds every errUnavailable call, and every newError with status 501 or
// 503 and an empty hint, in this package.
func TestRefusalEmittersExplainThemselves(t *testing.T) {
	found := hintlessRefusalSites(t)
	for _, site := range found {
		if !slices.Contains(hintlessRefusals, site) {
			t.Errorf("%q answers a 501 or 503 without a hint: give it one that says what to do (newError with a hint), or, if it is an internal fault that no deployment answers by design, add it to hintlessRefusals (internal/api/refusal_emitters_test.go)", site)
		}
	}
	for _, site := range hintlessRefusals {
		if !slices.Contains(found, site) {
			t.Errorf("hintlessRefusals names %q, which this package no longer answers: remove it", site)
		}
	}
	// errNotImplemented always carries its hint.
	if e := errNotImplemented("x"); e.Status != 501 || e.Body.Hint == nil || e.Body.Hint == "" {
		t.Errorf("errNotImplemented answers %d with hint %v", e.Status, e.Body.Hint)
	}
}

// hintlessRefusalSites returns the hintless 501 and 503 sites of the
// package's non-test files, sorted and without duplicates.
func hintlessRefusalSites(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var out []string
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, name, raw, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fn.Name.Name == "errUnavailable" {
				continue
			}
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
				case ident.Name == "newError" && len(call.Args) == 4 && refusalStatus(call.Args[0]) && emptyString(call.Args[3]):
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
				out = append(out, name+" "+fn.Name.Name+": "+text)
				return true
			})
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func refusalStatus(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	return ok && (sel.Sel.Name == "StatusServiceUnavailable" || sel.Sel.Name == "StatusNotImplemented")
}

func emptyString(e ast.Expr) bool {
	lit, ok := e.(*ast.BasicLit)
	return ok && lit.Kind == token.STRING && (lit.Value == `""` || lit.Value == "``")
}
