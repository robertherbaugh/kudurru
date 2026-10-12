package deploy_test

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/dnsserver"
)

// chartCase is one values permutation of charts/kudurru.
type chartCase struct {
	name   string
	values string // YAML
	// wantKind is the kind of the kudurru workload.
	wantKind string
	// check runs case-specific assertions.
	check func(t *testing.T, r *rendered)
}

const releaseNS = "kudurru-system"

// chartCases is the render matrix of the deploy package: every mode, the
// split Service, each edge mode with each store mode, each VIP provider,
// hostNetwork, persistence on and off, ingress, monitoring, network policy,
// encrypted transports and seeds. The CI helm job runs it and kubeconform.
var chartCases = []chartCase{
	{name: "default", wantKind: "Deployment", check: func(t *testing.T, r *rendered) {
		r.wantStore(t, "kube")
		r.wantObject(t, "ClusterRole", "kudurru")
		r.wantObject(t, "Role", "kudurru")
		r.wantObject(t, "PodDisruptionBudget", "kudurru")
		r.wantObject(t, "Secret", "kudurru-encryption-key")
		r.wantObject(t, "Secret", "kudurru-admin")
		svc := r.object(t, "Service", "kudurru-dns")
		if n := len(list(dig(svc, "spec", "ports"))); n != 2 {
			t.Errorf("mixed dns Service has %d ports, want udp+tcp", n)
		}
		if dig(r.pod(t), "spec", "subdomain") != "kudurru-headless" {
			t.Errorf("Deployment pods need subdomain=kudurru-headless for <pod>.<headless> lookups")
		}
		if dig(r.pod(t), "spec", "dnsPolicy") != "Default" {
			t.Errorf("dnsPolicy must be Default for the DNS server itself")
		}
		if dig(r.pod(t), "spec", "securityContext", "sysctls", 0, "name") != "net.ipv4.ip_unprivileged_port_start" {
			t.Errorf("uid 65532 binding :53 needs the ip_unprivileged_port_start sysctl")
		}
	}},
	{name: "standalone", values: "mode: standalone", wantKind: "StatefulSet", check: func(t *testing.T, r *rendered) {
		r.wantStore(t, "sqlite")
		r.wantNoKind(t, "ClusterRole")
		r.wantNoKind(t, "Role")
		r.wantNoKind(t, "PodDisruptionBudget")
		w := r.workload(t)
		if dig(w, "spec", "replicas") != 1 {
			t.Errorf("standalone runs exactly one replica, got %v", dig(w, "spec", "replicas"))
		}
		if len(list(dig(w, "spec", "volumeClaimTemplates"))) != 1 {
			t.Errorf("standalone needs the data volumeClaimTemplate")
		}
		if dig(r.pod(t), "spec", "automountServiceAccountToken") != false {
			t.Errorf("standalone pods need no ServiceAccount token")
		}
	}},
	{name: "standalone-no-persistence", values: "mode: standalone\npersistence: {standalone: {enabled: false}}", wantKind: "StatefulSet", check: func(t *testing.T, r *rendered) {
		if len(list(dig(r.workload(t), "spec", "volumeClaimTemplates"))) != 0 {
			t.Errorf("persistence.standalone.enabled=false must not request a claim")
		}
		r.wantVolume(t, "data", "emptyDir")
	}},
	{name: "split-protocols", values: "service: {dns: {splitProtocols: true}}", wantKind: "Deployment", check: func(t *testing.T, r *rendered) {
		r.wantNoObject(t, "Service", "kudurru-dns")
		for _, n := range []string{"kudurru-dns-udp", "kudurru-dns-tcp"} {
			svc := r.object(t, "Service", n)
			if str(dig(svc, "metadata", "annotations", "metallb.io/allow-shared-ip")) == "" {
				t.Errorf("%s lacks metallb.io/allow-shared-ip", n)
			}
		}
	}},
	{name: "edge-deployment", values: "edge: {enabled: true}", wantKind: "Deployment", check: func(t *testing.T, r *rendered) {
		d := r.wantEdge(t, "Deployment")
		if slices.Contains(list(r.container(t, mapOf(dig(d, "spec", "template")), "dnsdist")["args"]), any("--dnsdist-user=953:953")) {
			t.Errorf("the default edge runs as 953 from the start; no root start")
		}
		r.wantObject(t, "PodDisruptionBudget", "kudurru-edge")
		r.wantObject(t, "Secret", "kudurru-proxy-protocol")
		cfg := r.config(t)
		if cfg.Listeners.DNS[0].Address != "0.0.0.0:5353" || cfg.Listeners.ProxyProtocolSecretFile == "" || len(cfg.Listeners.ProxyProtocolFrom) == 0 {
			t.Errorf("behind the edge kudurru listens on :5353 with the PROXYv2 CIDR and TLV secret, got %+v", cfg.Listeners)
		}
		args := r.edgeArgs(t)
		if !slices.Contains(args, "--backends-from=endpointslice:kudurru-system/kudurru-headless") {
			t.Errorf("edge args lack the endpointslice backends: %v", args)
		}
	}},
	{name: "edge-deployment-pod-cidr", values: "edge: {enabled: true, mode: deployment, existingSecret: my-proxy, proxyProtocolFrom: ['172.16.0.0/16', 'fd00::/64']}", wantKind: "Deployment"},
	{name: "edge-sidecar", values: "edge: {enabled: true, mode: sidecar}", wantKind: "Deployment", check: func(t *testing.T, r *rendered) {
		r.wantNoObject(t, "Deployment", "kudurru-edge")
		cfg := r.config(t)
		if cfg.Listeners.DNS[0].Address != "127.0.0.1:5353" || cfg.Listeners.ProxyProtocolSecretFile != "" || !slices.Equal(cfg.Listeners.ProxyProtocolFrom, []string{"127.0.0.1/32"}) {
			t.Errorf("sidecar: kudurru on loopback :5353, loopback PROXYv2 without a secret; got %+v", cfg.Listeners)
		}
		if r.container(t, r.pod(t), "dnsdist") == nil {
			t.Errorf("sidecar mode needs the dnsdist container in the kudurru pod")
		}
	}},
	{name: "edge-daemonset", values: "edge: {enabled: true, mode: daemonset}", wantKind: "Deployment", check: func(t *testing.T, r *rendered) {
		ds := r.wantEdge(t, "DaemonSet")
		// On the host network 953 cannot bind :53 (no sysctl): root start.
		if c := r.container(t, mapOf(dig(ds, "spec", "template")), "dnsdist"); dig(c, "securityContext", "runAsUser") != 0 {
			t.Errorf("the hostNetwork edge must start dnsdist as root and drop to 953")
		}
	}},
	{name: "edge-ebpf", values: "edge: {enabled: true, dynblocks: {enabled: true}, ebpf: {enabled: true}}", wantKind: "Deployment", check: func(t *testing.T, r *rendered) {
		d := r.wantEdge(t, "Deployment")
		pod := mapOf(dig(d, "spec", "template"))
		c := r.container(t, pod, "dnsdist")
		caps := list(dig(c, "securityContext", "capabilities", "add"))
		if dig(c, "securityContext", "runAsUser") != 0 || !slices.Contains(caps, any("BPF")) || !slices.Contains(caps, any("SYS_ADMIN")) {
			t.Errorf("eBPF: dnsdist starts as root with BPF and SYS_ADMIN, got %v", c["securityContext"])
		}
		if dig(pod, "spec", "securityContext", "runAsNonRoot") != false {
			t.Errorf("eBPF: the edge pod cannot require runAsNonRoot")
		}
		if !slices.Contains(list(c["args"]), any("--ebpf")) {
			t.Errorf("eBPF: args lack --ebpf")
		}
	}},
	{name: "edge-ebpf-sidecar", values: "edge: {enabled: true, mode: sidecar, ebpf: {enabled: true}}", wantKind: "Deployment", check: func(t *testing.T, r *rendered) {
		pod := r.pod(t)
		if dig(r.container(t, pod, "dnsdist"), "securityContext", "runAsUser") != 0 {
			t.Errorf("eBPF sidecar: dnsdist starts as root")
		}
		if dig(r.container(t, pod, "kudurru"), "securityContext", "runAsUser") != 65532 {
			t.Errorf("the kudurru container stays uid 65532 next to a root dnsdist")
		}
	}},
	{name: "edge-sidecar-host-network", values: "edge: {enabled: true, mode: sidecar}\nhostNetwork: {enabled: true}", wantKind: "DaemonSet", check: func(t *testing.T, r *rendered) {
		if dig(r.container(t, r.pod(t), "dnsdist"), "securityContext", "runAsUser") != 0 {
			t.Errorf("a dnsdist sidecar on the host network starts as root")
		}
	}},
	{name: "edge-deployment-standalone", values: "mode: standalone\nedge: {enabled: true}", wantKind: "StatefulSet", check: func(t *testing.T, r *rendered) {
		r.wantEdge(t, "Deployment")
		r.wantStore(t, "sqlite")
	}},
	{name: "edge-sidecar-standalone", values: "mode: standalone\nedge: {enabled: true, mode: sidecar}", wantKind: "StatefulSet"},
	{name: "edge-daemonset-standalone", values: "mode: standalone\nedge: {enabled: true, mode: daemonset}", wantKind: "StatefulSet", check: func(t *testing.T, r *rendered) {
		r.wantEdge(t, "DaemonSet")
	}},
	{name: "central-database", values: "database: {existingSecret: pg-app}\nconfig: {settings: {database: {querylog: {facts: false}}}}", wantKind: "Deployment", check: func(t *testing.T, r *rendered) {
		r.wantVolume(t, "database", "secret")
		if !slices.Contains(r.envNames(t), "KUDURRU_DATABASE__DSN_FILE") {
			t.Errorf("no KUDURRU_DATABASE__DSN_FILE with database.existingSecret")
		}
	}},
	{name: "vip-none", values: "vip: {provider: none}", wantKind: "Deployment"},
	{name: "vip-kube-vip", values: "vip: {provider: kube-vip, address: 10.0.0.53}", wantKind: "Deployment", check: func(t *testing.T, r *rendered) {
		if v := dig(r.object(t, "Service", "kudurru-dns"), "metadata", "annotations", "kube-vip.io/loadbalancerIPs"); v != "10.0.0.53" {
			t.Errorf("kube-vip annotation = %v", v)
		}
	}},
	{name: "vip-metallb-split", values: "vip: {provider: metallb, address: 10.0.0.53, metallbPool: lan}\nservice: {dns: {splitProtocols: true}}", wantKind: "Deployment", check: func(t *testing.T, r *rendered) {
		for _, n := range []string{"kudurru-dns-udp", "kudurru-dns-tcp"} {
			a := mapOf(dig(r.object(t, "Service", n), "metadata", "annotations"))
			if a["metallb.io/loadBalancerIPs"] != "10.0.0.53" || a["metallb.io/address-pool"] != "lan" || a["metallb.io/allow-shared-ip"] == nil {
				t.Errorf("%s: MetalLB annotations = %v", n, a)
			}
		}
	}},
	{name: "vip-keepalived", values: "edge: {enabled: true}\nvip: {provider: keepalived, address: 10.0.0.53/24, keepalived: {interface: eth0}}", wantKind: "Deployment", check: func(t *testing.T, r *rendered) {
		ds := r.wantEdge(t, "DaemonSet")
		pod := mapOf(dig(ds, "spec", "template"))
		if dig(pod, "spec", "hostNetwork") != true {
			t.Errorf("keepalived edge must use hostNetwork")
		}
		k := r.container(t, pod, "keepalived")
		if k == nil {
			t.Fatal("keepalived container missing")
		}
		caps := list(dig(k, "securityContext", "capabilities", "add"))
		for _, c := range []string{"NET_ADMIN", "NET_RAW", "NET_BROADCAST"} {
			if !slices.Contains(caps, any(c)) {
				t.Errorf("keepalived lacks %s", c)
			}
		}
		if !slices.Contains(list(k["args"]), any("--vip=dns=10.0.0.53/24@eth0#53")) {
			t.Errorf("keepalived args = %v", k["args"])
		}
		if dig(pod, "spec", "nodeSelector", "kudurru.io/vip") != "true" {
			t.Errorf("keepalived DaemonSet must run on the kudurru.io/vip nodes")
		}
		r.wantObject(t, "Secret", "kudurru-vrrp")
		r.wantObject(t, "ClusterRole", "kudurru-edge-nodes")
	}},
	{name: "host-network", values: "hostNetwork: {enabled: true}", wantKind: "DaemonSet", check: func(t *testing.T, r *rendered) {
		pod := r.pod(t)
		if dig(pod, "spec", "hostNetwork") != true || dig(pod, "spec", "dnsPolicy") != "ClusterFirstWithHostNet" {
			t.Errorf("hostNetwork pods need hostNetwork and ClusterFirstWithHostNet")
		}
		if dig(pod, "spec", "topologySpreadConstraints") != nil {
			t.Errorf("a DaemonSet needs no topology spread")
		}
		if dig(pod, "spec", "securityContext", "sysctls") != nil {
			t.Errorf("hostNetwork pods cannot set namespaced sysctls")
		}
		// Without the sysctl, :53 needs the file capability of the
		// -hostnetwork image, which needs NET_BIND_SERVICE in the bounding set.
		c := r.container(t, pod, "kudurru")
		if img := str(c["image"]); !strings.HasSuffix(img, "-hostnetwork") {
			t.Errorf("hostNetwork pods need the -hostnetwork image, got %s", img)
		}
		if !slices.Contains(list(dig(c, "securityContext", "capabilities", "add")), any("NET_BIND_SERVICE")) {
			t.Errorf("the -hostnetwork image needs NET_BIND_SERVICE added")
		}
	}},
	{name: "host-network-plain-image", values: "hostNetwork: {enabled: true, imageTagSuffix: ''}\nimage: {tag: 9.9.9}", wantKind: "DaemonSet", check: func(t *testing.T, r *rendered) {
		if img := str(r.container(t, r.pod(t), "kudurru")["image"]); img != "ghcr.io/robertherbaugh/kudurru:9.9.9" {
			t.Errorf("imageTagSuffix '' keeps the plain tag, got %s", img)
		}
	}},
	{name: "host-network-standalone", values: "mode: standalone\nhostNetwork: {enabled: true}", wantKind: "StatefulSet"},
	{name: "persistence-querylog", values: "persistence: {querylog: {enabled: true, size: 20Gi}}", wantKind: "StatefulSet", check: func(t *testing.T, r *rendered) {
		vct := mapOf(dig(r.workload(t), "spec", "volumeClaimTemplates", 0))
		if dig(vct, "spec", "resources", "requests", "storage") != "20Gi" {
			t.Errorf("querylog claim = %v", vct)
		}
		r.wantStore(t, "kube")
	}},
	{name: "persistence-local", values: "persistence: {local: {enabled: true}}\npriorityClassName: system-cluster-critical", wantKind: "StatefulSet", check: func(t *testing.T, r *rendered) {
		if dig(r.pod(t), "spec", "priorityClassName") != "system-cluster-critical" {
			t.Errorf("priorityClassName not set")
		}
	}},
	{name: "ingress", values: "ingress: {enabled: true, className: nginx, host: dns.example.com, tls: [{secretName: dns-tls, hosts: [dns.example.com]}]}", wantKind: "Deployment", check: func(t *testing.T, r *rendered) {
		ing := r.object(t, "Ingress", "kudurru")
		if dig(ing, "spec", "rules", 0, "host") != "dns.example.com" {
			t.Errorf("ingress host = %v", dig(ing, "spec", "rules", 0, "host"))
		}
	}},
	{name: "monitoring", values: "serviceMonitor: {enabled: true}\npodMonitor: {enabled: true}\nprometheusRule: {enabled: true}\ngrafanaDashboard: {enabled: true}", wantKind: "Deployment", check: func(t *testing.T, r *rendered) {
		r.wantObject(t, "ServiceMonitor", "kudurru")
		r.wantObject(t, "PodMonitor", "kudurru")
		rule := r.object(t, "PrometheusRule", "kudurru")
		if len(list(dig(rule, "spec", "groups"))) == 0 {
			t.Errorf("PrometheusRule has no groups")
		}
		cm := r.object(t, "ConfigMap", "kudurru-dashboard")
		if !strings.Contains(str(dig(cm, "data", "kudurru-overview.json")), `"uid": "kudurru-overview"`) {
			t.Errorf("dashboard ConfigMap does not carry the dashboard JSON")
		}
	}},
	{name: "monitoring-edge", values: "serviceMonitor: {enabled: true, labels: {release: prom}}\nedge: {enabled: true}", wantKind: "Deployment", check: func(t *testing.T, r *rendered) {
		// The supervisor's :8084 /metrics feeds the edge row and KudurruEdgeBackendDown.
		pm := r.object(t, "PodMonitor", "kudurru-edge")
		if dig(pm, "spec", "selector", "matchLabels", "app.kubernetes.io/component") != "edge" || dig(pm, "spec", "podMetricsEndpoints", 0, "port") != "edge-ready" {
			t.Errorf("edge PodMonitor = %v", pm["spec"])
		}
		if dig(pm, "metadata", "labels", "release") != "prom" {
			t.Errorf("the edge PodMonitor carries the monitor labels")
		}
		r.wantNoObject(t, "PodMonitor", "kudurru")
	}},
	{name: "monitoring-keepalived", values: "podMonitor: {enabled: true}\nedge: {enabled: true}\nvip: {provider: keepalived, address: 10.0.0.53/24, keepalived: {interface: eth0}}", wantKind: "Deployment", check: func(t *testing.T, r *rendered) {
		// `kudurru vip run` :8085 feeds the VIP row and KudurruVIPNoMaster.
		pm := r.object(t, "PodMonitor", "kudurru-edge")
		if dig(pm, "spec", "podMetricsEndpoints", 1, "port") != "vip-health" {
			t.Errorf("keepalived: the edge PodMonitor must scrape vip-health, got %v", dig(pm, "spec", "podMetricsEndpoints"))
		}
	}},
	{name: "monitoring-sidecar", values: "podMonitor: {enabled: true}\nedge: {enabled: true, mode: sidecar}\nnetworkPolicy: {enabled: true}", wantKind: "Deployment", check: func(t *testing.T, r *rendered) {
		pm := r.object(t, "PodMonitor", "kudurru-edge")
		if dig(pm, "spec", "selector", "matchLabels", "app.kubernetes.io/component") != "dns" {
			t.Errorf("sidecar: the edge PodMonitor selects the kudurru pods, got %v", dig(pm, "spec", "selector"))
		}
		admitted := false
		for _, rule := range list(dig(r.object(t, "NetworkPolicy", "kudurru"), "spec", "ingress")) {
			for _, p := range list(mapOf(rule)["ports"]) {
				admitted = admitted || mapOf(p)["port"] == 8084
			}
		}
		if !admitted {
			t.Errorf("sidecar: the NetworkPolicy must admit the supervisor's :8084 (probes, metrics)")
		}
	}},
	{name: "network-policy", values: "networkPolicy: {enabled: true}", wantKind: "Deployment", check: func(t *testing.T, r *rendered) {
		np := r.object(t, "NetworkPolicy", "kudurru")
		r.wantPortOnlyFrom(t, np, 8082, "dns")
	}},
	{name: "network-policy-edge", values: "networkPolicy: {enabled: true}\nedge: {enabled: true}", wantKind: "Deployment", check: func(t *testing.T, r *rendered) {
		np := r.object(t, "NetworkPolicy", "kudurru")
		r.wantPortOnlyFrom(t, np, 5353, "edge")
		r.wantPortOnlyFrom(t, np, 8082, "dns", "edge")
		r.wantObject(t, "NetworkPolicy", "kudurru-edge")
	}},
	{name: "network-policy-keepalived", values: "networkPolicy: {enabled: true}\nedge: {enabled: true}\nvip: {provider: keepalived, address: 10.0.0.53/24, keepalived: {interface: eth0}}", wantKind: "Deployment"},
	{name: "network-policy-sidecar", values: "networkPolicy: {enabled: true}\nedge: {enabled: true, mode: sidecar}", wantKind: "Deployment"},
	{name: "encrypted-transports", values: "tls: {secretName: dns-tls}\nservice: {dot: {enabled: true}, doh: {enabled: true}, doq: {enabled: true}, doh3: {enabled: true}}", wantKind: "Deployment", check: func(t *testing.T, r *rendered) {
		cfg := r.config(t)
		if len(cfg.Listeners.DoT) != 1 || len(cfg.Listeners.DoH) != 1 || len(cfg.Listeners.DoQ) != 1 || len(cfg.Listeners.DoH3) != 1 {
			t.Errorf("encrypted listeners missing: %+v", cfg.Listeners)
		}
		if n := len(list(dig(r.object(t, "Service", "kudurru-dns"), "spec", "ports"))); n != 6 {
			t.Errorf("dns Service has %d ports, want 6", n)
		}
	}},
	{name: "encrypted-transports-edge", values: "edge: {enabled: true, tlsSecretName: dns-tls}\nservice: {dot: {enabled: true}, doh: {enabled: true}, doq: {enabled: true}, doh3: {enabled: true}}", wantKind: "Deployment", check: func(t *testing.T, r *rendered) {
		if cfg := r.config(t); len(cfg.Listeners.DoT) != 0 {
			t.Errorf("behind the edge, dnsdist terminates TLS; kudurru must not listen on DoT")
		}
		if !slices.Contains(r.edgeArgs(t), "--dot=0.0.0.0:853") {
			t.Errorf("edge lacks --dot: %v", r.edgeArgs(t))
		}
	}},
	{name: "external-dns-webhook", values: "externalDNS: {webhook: {enabled: true}}", wantKind: "Deployment", check: func(t *testing.T, r *rendered) {
		if !strings.Contains(r.nodeFile(t), "listen: :8888") {
			t.Errorf("node file lacks externalDNS.webhook.listen")
		}
	}},
	{name: "existing-secrets", values: "auth: {encryptionKeySecret: my-key, encryptionKeyPreviousSecret: my-old-key, bootstrapAdminSecret: my-admin, bootstrapToken: {enabled: true, existingSecret: my-token}}\nedge: {enabled: true, existingSecret: my-proxy}", wantKind: "Deployment", check: func(t *testing.T, r *rendered) {
		r.wantNoKind(t, "Secret")
		env := r.envNames(t)
		for _, want := range []string{"KUDURRU_ENCRYPTION_KEY_PREVIOUS_FILE", "KUDURRU_BOOTSTRAP_TOKEN_FILE"} {
			if !slices.Contains(env, want) {
				t.Errorf("env lacks %s", want)
			}
		}
	}},
	{name: "oidc", values: "auth: {oidc: {providers: [{id: corp, issuer: 'https://login.example.com', clientID: kudurru, clientSecretRef: {name: kudurru-oidc, key: client-secret}, redirectURL: 'https://dns.example.com/api/v1/auth/oidc/corp/callback'}]}}", wantKind: "Deployment", check: func(t *testing.T, r *rendered) {
		node := r.nodeFile(t)
		for _, want := range []string{"identityProviders:", "issuer: https://login.example.com", "name: kudurru-oidc"} {
			if !strings.Contains(node, want) {
				t.Errorf("node file lacks %q:\n%s", want, node)
			}
		}
	}},
	{name: "bootstrap-token", values: "auth: {bootstrapToken: {enabled: true}}", wantKind: "Deployment", check: func(t *testing.T, r *rendered) {
		r.wantObject(t, "Secret", "kudurru-bootstrap-token")
	}},
	{name: "seed-objects", values: seedObjects, wantKind: "Deployment", check: func(t *testing.T, r *rendered) {
		z := r.object(t, "DNSZone", "home")
		if dig(z, "metadata", "annotations", "helm.sh/hook") != "post-install" || dig(z, "metadata", "namespace") != nil {
			t.Errorf("DNSZone seed: %v", z["metadata"])
		}
		if dig(r.object(t, "BlockList", "ads"), "metadata", "namespace") != releaseNS {
			t.Errorf("namespaced seeds default to the release namespace")
		}
	}},
	{name: "seed-objects-standalone", values: "mode: standalone\n" + seedObjects, wantKind: "StatefulSet", check: func(t *testing.T, r *rendered) {
		r.wantNoKind(t, "DNSZone")
		if cfg := r.config(t); len(cfg.Objects) != 2 {
			t.Errorf("standalone seeds go into the node file's inline objects, got %d", len(cfg.Objects))
		}
	}},
	{name: "settings", values: "config:\n  node: {log: {level: debug}, listeners: {sockets: 2}}\n  settings:\n    cache: {maxEntries: 50000}\n    blocking: {defaultResponse: nxdomain}\n    upstreams: {groups: {default: {servers: ['tls://1.1.1.1'], strategy: strict}}}", wantKind: "Deployment", check: func(t *testing.T, r *rendered) {
		cfg := r.config(t)
		if cfg.Log.Level != "debug" || cfg.Listeners.Sockets != 2 {
			t.Errorf("config.node not applied: %+v", cfg)
		}
		if cfg.Cache.MaxEntries == 50000 || cfg.Blocking.DefaultResponse == "nxdomain" {
			t.Errorf("config.settings leaked into the node file; the binary reads the settings file (D106)")
		}
		settings := str(dig(r.object(t, "ConfigMap", "kudurru-settings"), "data", "settings.yaml"))
		if !strings.Contains(settings, "maxEntries: 50000") {
			t.Errorf("settings ConfigMap lacks the cluster keys")
		}
		// The binary layers the mounted settings file over the node file.
		sf := filepath.Join(t.TempDir(), "settings.yaml")
		if err := os.WriteFile(sf, []byte(settings), 0o600); err != nil {
			t.Fatal(err)
		}
		merged := checkConfig(t, "node file + settings ConfigMap", []byte(r.nodeFile(t)), []string{"KUDURRU_SETTINGS_FILE=" + sf})
		if merged.Cache.MaxEntries != 50000 || merged.Blocking.DefaultResponse != "nxdomain" || merged.Upstreams.Groups["default"].Strategy != "strict" {
			t.Errorf("settings file not applied over the node file: %+v", merged)
		}
	}},
	{name: "block-page", values: "blockPage: {enabled: true, ipv4: 192.0.2.80, https: {enabled: true}, service: {annotations: {metallb.io/loadBalancerIPs: 192.0.2.80}}}\nnetworkPolicy: {enabled: true}\nedge: {enabled: true}\nconfig: {settings: {blocking: {defaultResponse: blockPage}}}", wantKind: "Deployment", check: func(t *testing.T, r *rendered) {
		svc := r.object(t, "Service", "kudurru-blockpage")
		if dig(svc, "spec", "loadBalancerIP") != "192.0.2.80" || len(list(dig(svc, "spec", "ports"))) != 2 {
			t.Errorf("block page Service: %v", dig(svc, "spec"))
		}
		if dig(svc, "spec", "selector", "app.kubernetes.io/component") == "edge" {
			t.Errorf("the block page Service must select the kudurru pods, not the edge")
		}
		if cfg := r.config(t); cfg.Listeners.BlockPage.HTTP != ":8090" || cfg.Listeners.BlockPage.HTTPS != ":8493" {
			t.Errorf("listeners.blockPage = %+v", cfg.Listeners.BlockPage)
		}
		settings := str(dig(r.object(t, "ConfigMap", "kudurru-settings"), "data", "settings.yaml"))
		sf := filepath.Join(t.TempDir(), "settings.yaml")
		if err := os.WriteFile(sf, []byte(settings), 0o600); err != nil {
			t.Fatal(err)
		}
		merged := checkConfig(t, "node file + settings ConfigMap", []byte(r.nodeFile(t)), []string{"KUDURRU_SETTINGS_FILE=" + sf})
		if !merged.BlockPage.Enabled || merged.BlockPage.IPv4 != "192.0.2.80" || merged.Blocking.DefaultResponse != "blockPage" {
			t.Errorf("block page settings: %+v %s", merged.BlockPage, merged.Blocking.DefaultResponse)
		}
		ports := []string{}
		for _, p := range list(dig(r.container(t, r.pod(t), "kudurru"), "ports")) {
			ports = append(ports, str(mapOf(p)["name"]))
		}
		if !slices.Contains(ports, "blockpage-http") || !slices.Contains(ports, "blockpage-tls") {
			t.Errorf("container ports %v", ports)
		}
		if np := r.object(t, "NetworkPolicy", "kudurru"); !strings.Contains(fmt.Sprint(dig(np, "spec", "ingress")), "8090") {
			t.Errorf("NetworkPolicy does not admit the block page port")
		}
	}},
	{name: "everything", values: "edge: {enabled: true, tlsSecretName: dns-tls}\nservice: {dot: {enabled: true}, doh: {enabled: true}}\nvip: {provider: metallb, address: 192.0.2.53}\ningress: {enabled: true, host: dns.example.com}\nserviceMonitor: {enabled: true}\nprometheusRule: {enabled: true}\ngrafanaDashboard: {enabled: true}\nnetworkPolicy: {enabled: true, apiFrom: [{namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: ingress-nginx}}}]}\npriorityClassName: system-cluster-critical\nexternalDNS: {webhook: {enabled: true}}\nauth: {bootstrapToken: {enabled: true}}", wantKind: "Deployment"},
}

const seedObjects = `objects:
  - apiVersion: kudurru.io/v1alpha1
    kind: DNSZone
    metadata: {name: home}
    spec: {zoneName: home.arpa., type: primary}
  - apiVersion: kudurru.io/v1alpha1
    kind: BlockList
    metadata: {name: ads}
    spec: {mode: block, sources: [{url: "https://raw.githubusercontent.com/StevenBlack/hosts/master/hosts"}]}
`

// failCases must not render.
var failCases = []struct{ name, values, want string }{
	{"bad-mode", "mode: docker", "mode must be"},
	{"bad-vip", "vip: {provider: bgp}", "vip.provider must be"},
	{"bad-edge-mode", "edge: {enabled: true, mode: host}", "edge.mode must be"},
	{"keepalived-without-edge", "vip: {provider: keepalived, address: 10.0.0.53/24, keepalived: {interface: eth0}}", "needs edge.enabled"},
	{"keepalived-sidecar", "edge: {enabled: true, mode: sidecar}\nvip: {provider: keepalived, address: 10.0.0.53/24, keepalived: {interface: eth0}}", "sidecar"},
	{"keepalived-without-address", "edge: {enabled: true}\nvip: {provider: keepalived, keepalived: {interface: eth0}}", "vip.address"},
	{"keepalived-not-cidr", "edge: {enabled: true}\nvip: {provider: keepalived, address: 10.0.0.53, keepalived: {interface: eth0}}", "CIDR"},
	{"keepalived-without-interface", "edge: {enabled: true}\nvip: {provider: keepalived, address: 10.0.0.53/24}", "interface"},
	{"metallb-without-address", "vip: {provider: metallb}", "vip.address"},
	{"dot-without-tls", "service: {dot: {enabled: true}}", "tls.secretName"},
	{"dot-edge-without-tls", "edge: {enabled: true}\nservice: {doh: {enabled: true}}", "edge.tlsSecretName"},
	{"hostnetwork-without-net-bind-service", "hostNetwork: {enabled: true}\nsecurityContext: {capabilities: {drop: [ALL], add: []}}", "NET_BIND_SERVICE"},
	{"hostnetwork-persistence", "hostNetwork: {enabled: true}\npersistence: {querylog: {enabled: true}}", "hostNetwork DaemonSet"},
	{"ingress-without-host", "ingress: {enabled: true}", "ingress.host"},
	{"short-proxy-secret", "edge: {enabled: true, proxyProtocolSecret: short}", "at least 32 bytes"},
	{"edge-backend-53", "edge: {enabled: true, backendPort: 53}", "backendPort"},
	{"block-page-without-address", "blockPage: {enabled: true}", "blockPage.ipv4 or blockPage.ipv6"},
}

// helmBin returns the helm binary or skips the test.
func helmBin(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("helm")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("helm is required in CI")
		}
		t.Skip("helm not installed; the CI helm job runs the render matrix")
	}
	return p
}

// renderChart runs helm template for a values document.
func renderChart(t *testing.T, values string) ([]byte, error) {
	t.Helper()
	root := repoRoot(t)
	args := []string{"template", "kudurru", filepath.Join(root, "charts/kudurru"), "--namespace", releaseNS}
	if values != "" {
		f := filepath.Join(t.TempDir(), "values.yaml")
		if err := os.WriteFile(f, []byte(values), 0o600); err != nil {
			t.Fatal(err)
		}
		args = append(args, "--values", f)
	}
	cmd := exec.Command(helmBin(t), args...) //nolint:gosec // fixed binary, test arguments
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, stderr.String())
	}
	return out, nil
}

func TestChartRenderMatrix(t *testing.T) {
	for _, tc := range chartCases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := renderChart(t, tc.values)
			if err != nil {
				t.Fatalf("helm template: %v", err)
			}
			r := newRendered(t, out)
			r.checkGeneric(t)
			if k := r.workloadKind(t); k != tc.wantKind {
				t.Errorf("kudurru workload is a %s, want %s", k, tc.wantKind)
			}
			listeners := r.config(t).Listeners
			if listeners.ProxyProtocolSecretFile != "" {
				// Stand in for the Secret mounted in the pod, then run
				// the same policy construction as kudurru serve.
				listeners.ProxyProtocolSecretFile = filepath.Join(t.TempDir(), "proxy-secret")
				if err := os.WriteFile(listeners.ProxyProtocolSecretFile, []byte(strings.Repeat("0123456789abcdef", 4)), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			policy, err := dnsserver.PolicyFromConfig(listeners)
			if err != nil {
				t.Fatalf("DNS policy rejects rendered listeners: %v", err)
			}
			if policy.Enabled() != (len(listeners.ProxyProtocolFrom) > 0) {
				t.Fatal("DNS policy enabled state does not match proxyProtocolFrom")
			}
			if tc.check != nil {
				tc.check(t, r)
			}
		})
	}
}

func TestChartRejectsInvalidValues(t *testing.T) {
	for _, tc := range failCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := renderChart(t, tc.values)
			if err == nil {
				t.Fatalf("values rendered; want a failure mentioning %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// TestChartChecksumCoversNodeFileOnly pins D25/D106: a change to the node
// file rolls the pods; settings keys (the hot-reloaded settings ConfigMap)
// and anything else leave the template alone.
func TestChartChecksumCoversNodeFileOnly(t *testing.T) {
	checksum := func(values string) string {
		out, err := renderChart(t, values)
		if err != nil {
			t.Fatal(err)
		}
		r := newRendered(t, out)
		return str(dig(r.pod(t), "metadata", "annotations", "checksum/node-config"))
	}
	base := checksum("")
	if base == "" {
		t.Fatal("no checksum/node-config annotation")
	}
	if got := checksum("config: {node: {log: {level: debug}}}"); got == base {
		t.Errorf("a node key change must roll the pods")
	}
	if got := checksum("serviceMonitor: {enabled: true}\nresources: {requests: {cpu: 200m}}"); got != base {
		t.Errorf("changes outside the node file must not change the checksum")
	}
	if got := checksum("config: {settings: {cache: {maxEntries: 1}}}"); got != base {
		t.Errorf("a settings change must not roll the pods (D106)")
	}
}

// TestChartLint runs helm lint on both charts.
func TestChartLint(t *testing.T) {
	root := repoRoot(t)
	for _, c := range []string{"charts/kudurru", "charts/kudurru-crds"} {
		cmd := exec.Command(helmBin(t), "lint", "--strict", filepath.Join(root, c)) //nolint:gosec // fixed binary
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("helm lint %s: %v\n%s", c, err, out)
		}
	}
}

// TestCRDChartMatchesBases keeps charts/kudurru-crds/templates equal to
// config/crd/bases (make manifests is the source) plus the
// helm.sh/resource-policy: keep annotation that stops `helm uninstall` from
// deleting the CRDs and with them every kudurru.io object.
func TestCRDChartMatchesBases(t *testing.T) {
	root := repoRoot(t)
	bases, err := filepath.Glob(filepath.Join(root, "config/crd/bases/*.yaml"))
	if err != nil || len(bases) == 0 {
		t.Fatalf("no CRD bases: %v", err)
	}
	const marker = "    controller-gen.kubebuilder.io/version:"
	for _, b := range bases {
		base, err := os.ReadFile(b) //nolint:gosec // repository file
		if err != nil {
			t.Fatal(err)
		}
		var want bytes.Buffer
		for _, line := range strings.SplitAfter(string(base), "\n") {
			want.WriteString(line)
			if strings.HasPrefix(line, marker) {
				want.WriteString("    helm.sh/resource-policy: keep\n")
			}
		}
		got, err := os.ReadFile(filepath.Join(root, "charts/kudurru-crds/templates", filepath.Base(b))) //nolint:gosec // repository file
		if err != nil || !bytes.Equal(got, want.Bytes()) {
			t.Errorf("charts/kudurru-crds/templates/%s differs from config/crd/bases; run make manifests", filepath.Base(b))
		}
		if !strings.Contains(string(got), "helm.sh/resource-policy: keep") {
			t.Errorf("charts/kudurru-crds/templates/%s lacks helm.sh/resource-policy: keep", filepath.Base(b))
		}
	}
}
