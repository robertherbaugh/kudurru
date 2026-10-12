package deploy_test

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/config"
)

// rendered is one helm template output with lookup helpers and the generic
// checks every permutation must pass.
type rendered struct {
	docs []map[string]any
	cfg  *config.Config
	// externalSecrets allows Secret volumes the render does not contain
	// (the kustomize tree expects pre-created Secrets).
	externalSecrets bool
}

var clusterScoped = map[string]bool{
	"ClusterRole": true, "ClusterRoleBinding": true, "DNSZone": true, "Namespace": true,
	"CustomResourceDefinition": true, "PriorityClass": true,
}

var workloadKinds = []string{"Deployment", "DaemonSet", "StatefulSet"}

func newRendered(t *testing.T, out []byte) *rendered {
	t.Helper()
	return &rendered{docs: decodeDocs(t, out)}
}

func (r *rendered) find(kind, name string) map[string]any {
	for _, d := range r.docs {
		if d["kind"] == kind && dig(d, "metadata", "name") == name {
			return d
		}
	}
	return nil
}

func (r *rendered) object(t *testing.T, kind, name string) map[string]any {
	t.Helper()
	o := r.find(kind, name)
	if o == nil {
		t.Fatalf("no %s/%s rendered", kind, name)
	}
	return o
}

func (r *rendered) wantObject(t *testing.T, kind, name string) { t.Helper(); r.object(t, kind, name) }

func (r *rendered) wantNoObject(t *testing.T, kind, name string) {
	t.Helper()
	if r.find(kind, name) != nil {
		t.Errorf("%s/%s must not be rendered", kind, name)
	}
}

func (r *rendered) wantNoKind(t *testing.T, kind string) {
	t.Helper()
	for _, d := range r.docs {
		if d["kind"] == kind {
			t.Errorf("unexpected %s/%v", kind, dig(d, "metadata", "name"))
		}
	}
}

// workload is the kudurru workload (named after the release).
func (r *rendered) workload(t *testing.T) map[string]any {
	t.Helper()
	for _, k := range workloadKinds {
		if o := r.find(k, "kudurru"); o != nil {
			return o
		}
	}
	t.Fatal("no kudurru workload rendered")
	return nil
}

func (r *rendered) workloadKind(t *testing.T) string { t.Helper(); return str(r.workload(t)["kind"]) }

// pod is the kudurru pod template.
func (r *rendered) pod(t *testing.T) map[string]any {
	t.Helper()
	return mapOf(dig(r.workload(t), "spec", "template"))
}

func (r *rendered) container(t *testing.T, pod map[string]any, name string) map[string]any {
	t.Helper()
	for _, c := range list(dig(pod, "spec", "containers")) {
		if cm := mapOf(c); cm["name"] == name {
			return cm
		}
	}
	return nil
}

func (r *rendered) nodeFile(t *testing.T) string {
	t.Helper()
	return str(dig(r.object(t, "ConfigMap", "kudurru-node"), "data", "kudurru.yaml"))
}

// fileStandIns are the contents of the Secret files the chart mounts for
// KUDURRU_<SECTION>__<KEY>_FILE variables, valid for their keys.
var fileStandIns = map[string]string{
	"KUDURRU_DATABASE__DSN_FILE": "postgres://kudurru@db.example:5432/kudurru?sslmode=verify-full",
}

// config loads the node file with the kudurru container's settings env.
func (r *rendered) config(t *testing.T) *config.Config {
	t.Helper()
	if r.cfg != nil {
		return r.cfg
	}
	c := r.container(t, r.pod(t), "kudurru")
	if c == nil {
		t.Fatal("no kudurru container")
	}
	var env []string
	for _, e := range list(c["env"]) {
		em := mapOf(e)
		name := str(em["name"])
		if !strings.HasPrefix(name, config.EnvPrefix) || !strings.Contains(name, "__") {
			continue
		}
		v := str(em["value"])
		if em["valueFrom"] != nil {
			v = "kudurru-0"
		}
		// A key read from a mounted Secret: stand in for the file.
		if strings.HasSuffix(name, "_FILE") {
			f := filepath.Join(t.TempDir(), "secret")
			if err := os.WriteFile(f, []byte(fileStandIns[name]), 0o600); err != nil {
				t.Fatal(err)
			}
			v = f
		}
		env = append(env, name+"="+v)
	}
	r.cfg = checkConfig(t, "node ConfigMap", []byte(r.nodeFile(t)), env)
	return r.cfg
}

func (r *rendered) wantStore(t *testing.T, driver string) {
	t.Helper()
	if got := r.config(t).Store.Driver; got != driver {
		t.Errorf("store.driver = %s, want %s", got, driver)
	}
}

func (r *rendered) wantVolume(t *testing.T, name, source string) {
	t.Helper()
	for _, v := range list(dig(r.pod(t), "spec", "volumes")) {
		if vm := mapOf(v); vm["name"] == name {
			if _, ok := vm[source]; !ok {
				t.Errorf("volume %s is not a %s: %v", name, source, vm)
			}
			return
		}
	}
	t.Errorf("volume %s missing", name)
}

// wantEdge asserts the edge workload of kind and that the dns Service(s)
// select edge pods.
func (r *rendered) wantEdge(t *testing.T, kind string) map[string]any {
	t.Helper()
	e := r.object(t, kind, "kudurru-edge")
	for _, d := range r.docs {
		if d["kind"] == "Service" && strings.HasPrefix(str(dig(d, "metadata", "labels", "kudurru.io/service")), "dns") {
			if dig(d, "spec", "selector", "app.kubernetes.io/component") != "edge" {
				t.Errorf("Service %v must select the edge pods", dig(d, "metadata", "name"))
			}
		}
	}
	r.wantObject(t, "Role", "kudurru-edge")
	return e
}

func (r *rendered) edgeArgs(t *testing.T) []string {
	t.Helper()
	var pod map[string]any
	for _, k := range []string{"Deployment", "DaemonSet"} {
		if o := r.find(k, "kudurru-edge"); o != nil {
			pod = mapOf(dig(o, "spec", "template"))
		}
	}
	if pod == nil {
		pod = r.pod(t)
	}
	c := r.container(t, pod, "dnsdist")
	if c == nil {
		t.Fatal("no dnsdist container")
	}
	var out []string
	for _, a := range list(c["args"]) {
		out = append(out, str(a))
	}
	return out
}

func (r *rendered) envNames(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, e := range list(r.container(t, r.pod(t), "kudurru")["env"]) {
		out = append(out, str(mapOf(e)["name"]))
	}
	return out
}

// wantPortOnlyFrom asserts that every ingress rule of np that opens port
// admits only pods with the given component labels.
func (r *rendered) wantPortOnlyFrom(t *testing.T, np map[string]any, port int, components ...string) {
	t.Helper()
	found := false
	for _, rule := range list(dig(np, "spec", "ingress")) {
		rm := mapOf(rule)
		opens := false
		for _, p := range list(rm["ports"]) {
			if dig(p, "port") == port {
				opens = true
			}
		}
		if !opens {
			continue
		}
		found = true
		from := list(rm["from"])
		if len(from) == 0 {
			t.Errorf("port %d is open to everyone", port)
		}
		var got []string
		for _, peer := range from {
			if c := str(dig(peer, "podSelector", "matchLabels", "app.kubernetes.io/component")); c != "" {
				got = append(got, c)
			}
		}
		for _, c := range components {
			if !slices.Contains(got, c) {
				t.Errorf("port %d: no rule admits %s pods (got %v)", port, c, got)
			}
		}
		for _, c := range got {
			if !slices.Contains(components, c) {
				t.Errorf("port %d admits %s pods", port, c)
			}
		}
	}
	if !found {
		t.Errorf("no ingress rule for port %d", port)
	}
}

// checkGeneric runs the structural checks every permutation must pass.
func (r *rendered) checkGeneric(t *testing.T) {
	t.Helper()
	seen := map[string]bool{}
	var pods []map[string]any // pod templates, for Service/PDB checks
	for _, d := range r.docs {
		kind, name := str(d["kind"]), str(dig(d, "metadata", "name"))
		if str(d["apiVersion"]) == "" || kind == "" || name == "" {
			t.Errorf("object without apiVersion/kind/name: %v", d["metadata"])
			continue
		}
		ns := str(dig(d, "metadata", "namespace"))
		if clusterScoped[kind] && ns != "" {
			t.Errorf("%s/%s is cluster-scoped but has namespace %s", kind, name, ns)
		}
		if !clusterScoped[kind] && ns != releaseNS {
			t.Errorf("%s/%s has namespace %q, want %s", kind, name, ns, releaseNS)
		}
		id := kind + "/" + ns + "/" + name
		if seen[id] {
			t.Errorf("%s rendered twice", id)
		}
		seen[id] = true
		if (kind == "Service" || kind == "ConfigMap") && len(name) > 63 {
			t.Errorf("%s/%s name longer than 63", kind, name)
		}
		if slices.Contains(workloadKinds, kind) {
			pod := mapOf(dig(d, "spec", "template"))
			pods = append(pods, pod)
			r.checkWorkload(t, d, pod)
		}
	}
	for _, d := range r.docs {
		switch d["kind"] {
		case "Service":
			r.checkService(t, d, pods)
		case "PodDisruptionBudget":
			if !anyPodMatches(mapOf(dig(d, "spec", "selector", "matchLabels")), pods) {
				t.Errorf("PDB %v selects no pods", dig(d, "metadata", "name"))
			}
		case "Ingress":
			for _, rule := range list(dig(d, "spec", "rules")) {
				for _, p := range list(dig(rule, "http", "paths")) {
					if svc := str(dig(p, "backend", "service", "name")); r.find("Service", svc) == nil {
						t.Errorf("Ingress backend %s not rendered", svc)
					}
				}
			}
		case "PodMonitor":
			// Every endpoint names a port of a selected pod.
			sel := mapOf(dig(d, "spec", "selector", "matchLabels"))
			ports := map[string]bool{}
			for _, p := range pods {
				if !labelsMatch(sel, mapOf(dig(p, "metadata", "labels"))) {
					continue
				}
				for _, c := range list(dig(p, "spec", "containers")) {
					for _, cp := range list(mapOf(c)["ports"]) {
						ports[str(mapOf(cp)["name"])] = true
					}
				}
			}
			if len(ports) == 0 {
				t.Errorf("PodMonitor %v selects no pods", dig(d, "metadata", "name"))
			}
			for _, e := range list(dig(d, "spec", "podMetricsEndpoints")) {
				if port := str(mapOf(e)["port"]); !ports[port] {
					t.Errorf("PodMonitor %v scrapes port %s, which no selected pod declares", dig(d, "metadata", "name"), port)
				}
			}
		case "ServiceMonitor":
			want := mapOf(dig(d, "spec", "selector", "matchLabels"))
			ok := false
			for _, s := range r.docs {
				if s["kind"] == "Service" && labelsMatch(want, mapOf(dig(s, "metadata", "labels"))) {
					ok = true
				}
			}
			if !ok {
				t.Errorf("ServiceMonitor selects no Service")
			}
		}
	}
}

func labelsMatch(selector, labels map[string]any) bool {
	if len(selector) == 0 {
		return false
	}
	for k, v := range selector {
		if labels[k] != v {
			return false
		}
	}
	return true
}

func anyPodMatches(selector map[string]any, pods []map[string]any) bool {
	for _, p := range pods {
		if labelsMatch(selector, mapOf(dig(p, "metadata", "labels"))) {
			return true
		}
	}
	return false
}

func (r *rendered) checkWorkload(t *testing.T, w, pod map[string]any) {
	t.Helper()
	name := fmt.Sprintf("%v/%v", w["kind"], dig(w, "metadata", "name"))
	if !labelsMatch(mapOf(dig(w, "spec", "selector", "matchLabels")), mapOf(dig(pod, "metadata", "labels"))) {
		t.Errorf("%s: selector does not match the pod template labels", name)
	}
	volumes := map[string]map[string]any{}
	for _, v := range list(dig(pod, "spec", "volumes")) {
		vm := mapOf(v)
		volumes[str(vm["name"])] = vm
		if _, ok := vm["hostPath"]; ok {
			t.Errorf("%s: hostPath volume %v", name, vm["name"])
		}
		if cm := str(dig(vm, "configMap", "name")); cm != "" && r.find("ConfigMap", cm) == nil {
			t.Errorf("%s: ConfigMap %s not rendered", name, cm)
		}
		if s := str(dig(vm, "secret", "secretName")); strings.HasPrefix(s, "kudurru") && !r.externalSecrets && r.find("Secret", s) == nil {
			t.Errorf("%s: chart-generated Secret %s not rendered", name, s)
		}
	}
	for _, vct := range list(dig(w, "spec", "volumeClaimTemplates")) {
		volumes[str(dig(vct, "metadata", "name"))] = map[string]any{}
	}
	hostNet := dig(pod, "spec", "hostNetwork") == true
	podSC := mapOf(dig(pod, "spec", "securityContext"))
	for _, c := range list(dig(pod, "spec", "containers")) {
		cm := mapOf(c)
		cname := str(cm["name"])
		for _, m := range list(cm["volumeMounts"]) {
			if _, ok := volumes[str(mapOf(m)["name"])]; !ok {
				t.Errorf("%s/%s: volumeMount %v has no volume", name, cname, mapOf(m)["name"])
			}
		}
		for _, e := range list(cm["env"]) {
			if err := checkEnvName(str(mapOf(e)["name"])); err != nil {
				t.Errorf("%s/%s: %v", name, cname, err)
			}
		}
		if cname == "keepalived" {
			continue // privileged by design (NET_ADMIN as root), documented
		}
		if sc := mapOf(cm["securityContext"]); cname == "dnsdist" && sc["runAsUser"] == 0 {
			checkEdgeRootStart(t, name+"/"+cname, cm)
			continue
		}
		checkRestricted(t, name+"/"+cname, podSC, mapOf(cm["securityContext"]), hostNet)
	}
}

// checkEdgeRootStart asserts the root start of the dnsdist container (eBPF,
// host network): every capability dropped except the ones dnsdist needs to
// bind, create the eBPF maps and switch to 953, which it is told to do.
func checkEdgeRootStart(t *testing.T, name string, c map[string]any) {
	t.Helper()
	sc := mapOf(c["securityContext"])
	if sc["allowPrivilegeEscalation"] != false || sc["readOnlyRootFilesystem"] != true || sc["runAsGroup"] != 953 {
		t.Errorf("%s: root start needs allowPrivilegeEscalation false, a read-only root and group 953: %v", name, sc)
	}
	if !slices.Contains(list(dig(sc, "capabilities", "drop")), any("ALL")) {
		t.Errorf("%s: capabilities must drop ALL", name)
	}
	allowed := []any{"NET_BIND_SERVICE", "SETUID", "SETGID", "KILL", "BPF", "SYS_ADMIN"}
	for _, c := range list(dig(sc, "capabilities", "add")) {
		if !slices.Contains(allowed, c) {
			t.Errorf("%s: capability %v added to the root dnsdist container", name, c)
		}
	}
	if !slices.Contains(list(c["args"]), any("--dnsdist-user=953:953")) {
		t.Errorf("%s: a root supervisor must start dnsdist with --dnsdist-user=953:953", name)
	}
}

// checkRestricted asserts the Restricted Pod Security Standard fields
// (hostNetwork itself is reported by the caller's case, not here).
func checkRestricted(t *testing.T, name string, podSC, sc map[string]any, _ bool) {
	t.Helper()
	if sc["runAsNonRoot"] != true && podSC["runAsNonRoot"] != true {
		t.Errorf("%s: runAsNonRoot is not true", name)
	}
	if sc["allowPrivilegeEscalation"] != false {
		t.Errorf("%s: allowPrivilegeEscalation must be false", name)
	}
	if sc["readOnlyRootFilesystem"] != true {
		t.Errorf("%s: readOnlyRootFilesystem must be true", name)
	}
	if dig(sc, "seccompProfile", "type") != "RuntimeDefault" && dig(podSC, "seccompProfile", "type") != "RuntimeDefault" {
		t.Errorf("%s: seccompProfile RuntimeDefault missing", name)
	}
	if !slices.Contains(list(dig(sc, "capabilities", "drop")), any("ALL")) {
		t.Errorf("%s: capabilities must drop ALL", name)
	}
	for _, c := range list(dig(sc, "capabilities", "add")) {
		if c != "NET_BIND_SERVICE" {
			t.Errorf("%s: capability %v is not allowed under Restricted", name, c)
		}
	}
}

func (r *rendered) checkService(t *testing.T, svc map[string]any, pods []map[string]any) {
	t.Helper()
	name := str(dig(svc, "metadata", "name"))
	selector := mapOf(dig(svc, "spec", "selector"))
	var matched []map[string]any
	for _, p := range pods {
		if labelsMatch(selector, mapOf(dig(p, "metadata", "labels"))) {
			matched = append(matched, p)
		}
	}
	if len(matched) == 0 {
		t.Errorf("Service %s selects no pods", name)
		return
	}
	for _, port := range list(dig(svc, "spec", "ports")) {
		pm := mapOf(port)
		target, ok := pm["targetPort"].(string)
		if !ok {
			continue
		}
		proto := str(pm["protocol"])
		found := false
		for _, p := range matched {
			for _, c := range list(dig(p, "spec", "containers")) {
				for _, cp := range list(mapOf(c)["ports"]) {
					cpm := mapOf(cp)
					if cpm["name"] == target && (proto == "" || cpm["protocol"] == proto) {
						found = true
					}
				}
			}
		}
		if !found {
			t.Errorf("Service %s: targetPort %s/%s is not a container port of the selected pods", name, target, proto)
		}
	}
}
