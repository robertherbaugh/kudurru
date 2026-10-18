package dnsdist

import (
	"fmt"
	"strings"

	"github.com/robertherbaugh/kudurru/ee/edge"
)

// LuaRenderer renders a Lua `dnsdist.conf`: the fallback should the v0.2
// spike show that YAML-started dnsdist cannot take console membership
// changes (D17). It expresses exactly the same Topology as YAMLRenderer.
// enableLuaConfiguration() is never rendered; it is meaningless in a Lua
// configuration and forbidden next to YAML.
type LuaRenderer struct{}

// Name implements Renderer.
func (LuaRenderer) Name() string { return "lua" }

// Main implements Renderer.
func (LuaRenderer) Main() string { return "dnsdist.conf" }

// Render implements Renderer.
func (r LuaRenderer) Render(in *edge.Topology) ([]File, error) {
	t, err := prepare(in)
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	w("-- %s", header)
	w("")
	w("-- clients")
	w("setACL(%s)", luaStrings(t.ACL))
	w("")
	w("-- console and webserver: loopback only, credentials generated per start")
	w("controlSocket(%s)", luaString(t.Console.Address))
	w("setKey(%s)", luaString(t.Console.Key))
	w("setConsoleACL(%s)", luaStrings(loopbackACL))
	w("webserver(%s)", luaString(t.Webserver.Address))
	w("setWebserverConfig({apiKey=%s, password=%s, hashPlaintextCredentials=true, acl=%s, apiRequiresAuthentication=true, statsRequireAuthentication=true, dashboardRequiresAuthentication=true})",
		luaString(t.Webserver.APIKey), luaString(t.Webserver.Password), luaString(strings.Join(loopbackACL, ", ")))
	w("setAPIWritable(false)")
	w("setServFailWhenNoServer(true)")
	if t.EBPF.Enabled {
		w("addCapabilitiesToRetain({\"CAP_SYS_ADMIN\"})")
	}
	w("")
	w("-- frontends")
	first := true
	for _, bd := range t.Binds {
		switch bd.Protocol {
		case edge.Do53:
			fn := "addLocal"
			if first {
				fn = "setLocal" // replaces the implicit 127.0.0.1:53
				first = false
			}
			opts := []string{}
			if bd.ReusePort {
				opts = append(opts, "reusePort=true")
			}
			w("%s(%s, {%s})", fn, luaString(bd.Address), strings.Join(opts, ", "))
			// Lua has no threads option: one listener per call, and
			// reusePort spreads the load across them.
			for i := 1; i < bd.Threads; i++ {
				w("addLocal(%s, {%s})", luaString(bd.Address), strings.Join(opts, ", "))
			}
		case edge.DoT:
			w("addTLSLocal(%s, %s, %s, {minTLSVersion=\"tls1.2\"})", luaString(bd.Address), luaString(bd.CertFile), luaString(bd.KeyFile))
		case edge.DoH:
			w("addDOHLocal(%s, %s, %s, %s, {minTLSVersion=\"tls1.2\"})", luaString(bd.Address), luaString(bd.CertFile), luaString(bd.KeyFile), luaStrings(bd.DoHPaths))
		case edge.DoQ:
			w("addDOQLocal(%s, %s, %s)", luaString(bd.Address), luaString(bd.CertFile), luaString(bd.KeyFile))
		case edge.DoH3:
			w("addDOH3Local(%s, %s, %s)", luaString(bd.Address), luaString(bd.CertFile), luaString(bd.KeyFile))
		}
	}
	w("")
	w("-- pools")
	for _, p := range t.Pools {
		w("setPoolServerPolicy(%s, %s)", p.Policy, luaString(p.Name))
		if c := p.Cache; c != nil {
			v := luaIdent(c.Name)
			w("local %s = newPacketCache(%d, {maxTTL=%d, minTTL=%d, staleTTL=%d, temporaryFailureTTL=%d, maxNegativeTTL=%d})",
				v, c.Size, c.MaxTTL, c.MinTTL, c.StaleTTL, c.TemporaryFailureTTL, c.MaxNegativeTTL)
			w("getPool(%s):setCache(%s)", luaString(p.Name), v)
		}
	}
	if v := luaValidators(t); v != "" {
		w("")
		w("-- health-check response validators (dnsdist 2.1)")
		b.WriteString(v)
	}
	w("")
	w("-- backends")
	hc := t.HealthCheck
	for _, be := range t.Backends {
		w("do")
		w("  local s = newServer({address=%s, id=%s, name=%s, pool=%s, useProxyProtocol=%t, weight=%d, order=%d, sockets=%d, healthCheckMode=\"auto\", checkName=%s, checkType=%s, checkInterval=%d, checkTimeout=%d, maxCheckFailures=%d, rise=%d, mustResolve=%t})",
			luaString(be.Address), luaString(be.ID), luaString(be.DisplayName()), luaStrings(be.Pools), be.ProxyProtocol,
			be.Weight, be.Order, be.Sockets, luaString(hc.QName), luaString(hc.QType), hc.Interval, hc.Timeout, hc.MaxFailures, hc.Rise, hc.MustResolve)
		if fn := validatorName(t, be); fn != "" {
			w("  s:setHealthCheckResponseValidator(%s)", fn)
		}
		w("end")
	}
	w("")
	w("-- query rules")
	refuse := "OrRule({OpcodeRule(DNSOpcode.Notify), OpcodeRule(DNSOpcode.Update), QTypeRule(DNSQType.AXFR), QTypeRule(DNSQType.IXFR)})"
	if len(t.Rules.TransferSources) > 0 {
		w("local kudurru_transfer_sources = newNMG()")
		for _, n := range t.Rules.TransferSources {
			w("kudurru_transfer_sources:addMask(%s)", luaString(n))
		}
		refuse = "AndRule({" + refuse + ", NotRule(NetmaskGroupRule(kudurru_transfer_sources))})"
	}
	w("addAction(%s, RCodeAction(DNSRCode.REFUSED), {name=%s, uuid=%s})", refuse, luaString(ruleRefuseTransfers), luaString(ruleUUID(ruleRefuseTransfers)))
	if t.ProxyProtocolSecret != "" {
		w("addAction(AllRule(), SetAdditionalProxyProtocolValueAction(%d, %s), {name=%s, uuid=%s})",
			edge.ProxyTLVSecret, luaString(t.ProxyProtocolSecret), luaString(ruleProxySecret), luaString(ruleUUID(ruleProxySecret)))
	}
	if len(t.Rules.AuthoritativeZones) > 0 {
		w("addAction(QNameSuffixRule(%s), PoolAction(%s), {name=%s, uuid=%s})",
			luaStrings(t.Rules.AuthoritativeZones), luaString(edge.PoolAuthoritative), luaString(ruleAuthoritative), luaString(ruleUUID(ruleAuthoritative)))
	}
	w("addAction(AllRule(), PoolAction(%s), {name=%s, uuid=%s})", luaString(edge.PoolResolver), luaString(ruleResolver), luaString(ruleUUID(ruleResolver)))
	if t.EBPF.Enabled {
		w("")
		w("-- eBPF socket filter (opt-in; DynBlocks drops are pushed into it)")
		w("local kudurru_bpf = newBPFFilter({ipv4MaxItems=%d, ipv6MaxItems=%d, qnamesMaxItems=%d})", t.EBPF.IPv4MaxItems, t.EBPF.IPv6MaxItems, t.EBPF.QNameMaxItems)
		w("setDefaultBPFFilter(kudurru_bpf)")
	}
	if d := t.DynBlocks; d.Enabled {
		w("")
		w("-- dynamic blocks (opt-in)")
		w("local kudurru_dbr = dynBlockRulesGroup()")
		if d.QueryRate > 0 {
			w("kudurru_dbr:setQueryRate(%d, %d, %s, %d, DNSAction.%s)", d.QueryRate, d.Window, luaString("kudurru: query rate exceeded"), d.BlockFor, d.Action)
		}
		if d.NXDomainRate > 0 {
			w("kudurru_dbr:setRCodeRate(DNSRCode.NXDOMAIN, %d, %d, %s, %d, DNSAction.%s)", d.NXDomainRate, d.Window, luaString("kudurru: NXDOMAIN rate exceeded"), d.BlockFor, d.Action)
		}
		w("kudurru_dbr:setMasks(32, 64, 0)")
		if len(d.Exclude) > 0 {
			w("kudurru_dbr:excludeRange(%s)", luaStrings(d.Exclude))
		}
		w("function maintenance()")
		w("  kudurru_dbr:apply()")
		w("end")
	}
	return []File{{Name: r.Main(), Data: []byte(b.String())}}, nil
}

// luaIdent turns a cache name into a Lua local variable name.
func luaIdent(s string) string {
	return "kudurru_" + strings.NewReplacer("-", "_", ".", "_").Replace(s)
}
