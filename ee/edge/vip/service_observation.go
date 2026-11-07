package vip

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// ServiceBinding is an explicit host-selected Service and address to observe.
// Neither an allocation annotation nor an ingress IP establishes an owner.
type ServiceBinding struct{ Name, Address, Namespace, Service string }

// AnnouncerEvidence describes provider-reported state, not a physical ARP/BGP
// proof. MetalLB L2 status has NO provider heartbeat: RenewedAt stays nil even
// after a successful reread. LivenessAt is a separate node heartbeat.
type AnnouncerEvidence struct {
	Kind            string     `json:"kind"`
	UID             string     `json:"uid"`
	ResourceVersion string     `json:"resourceVersion"`
	CreatedAt       time.Time  `json:"createdAt"`
	RenewedAt       *time.Time `json:"renewedAt,omitempty"`
	LivenessAt      *time.Time `json:"livenessAt,omitempty"`
}

// OwnerObservation expires from the ORIGINAL acquisition start and, for
// leased evidence, the original lease deadline. Acquisition cannot renew it.
type OwnerObservation struct {
	Name       string             `json:"name"`
	Address    string             `json:"address"`
	Owner      string             `json:"owner,omitempty"`
	Known      bool               `json:"known"`
	Reason     string             `json:"reason,omitempty"`
	CheckedAt  time.Time          `json:"checkedAt"`
	ExpiresAt  time.Time          `json:"expiresAt"`
	ServiceUID string             `json:"serviceUID,omitempty"`
	Evidence   *AnnouncerEvidence `json:"evidence,omitempty"`
}

// Observation is an immutable copy of the most recent bounded acquisition.
type Observation struct {
	Provider string             `json:"provider"`
	VIPs     []OwnerObservation `json:"vips"`
}

// ServiceObserverConfig bounds read-only acquisition. Only per-Service kube-vip
// ARP election and MetalLB ServiceL2Status are supported. Global election/BGP
// sources cannot establish a single current owner and remain unknown.
type ServiceObserverConfig struct {
	Kind                      string
	Bindings                  []ServiceBinding
	AnnouncerNamespace        string
	Interval, Timeout, MaxAge time.Duration
	Now                       func() time.Time
}

// ServiceObserver acquires provider evidence using installed Kubernetes read
// permissions. Its only runtime effect is replacing a local observation; it
// never allocates an address, writes a Lease, or calls Provider.SetWeight.
type ServiceObserver struct {
	*ServiceProvider
	client  dynamic.Interface
	cfg     ServiceObserverConfig
	refresh sync.Mutex
}

var (
	serviceGVR = schema.GroupVersionResource{Version: "v1", Resource: "services"}
	nodeGVR    = schema.GroupVersionResource{Version: "v1", Resource: "nodes"}
	podGVR     = schema.GroupVersionResource{Version: "v1", Resource: "pods"}
	leaseGVR   = schema.GroupVersionResource{Group: "coordination.k8s.io", Version: "v1", Resource: "leases"}
	l2GVR      = schema.GroupVersionResource{Group: "metallb.io", Version: "v1beta1", Resource: "servicel2statuses"}
)

// NewServiceObserver captures bindings, bounds and the installed read client.
func NewServiceObserver(client dynamic.Interface, cfg ServiceObserverConfig) (*ServiceObserver, error) {
	if client == nil || len(cfg.Bindings) == 0 || len(cfg.Bindings) > 32 {
		return nil, errors.New("vip observation: client and 1-32 bindings required")
	}
	if cfg.Interval < time.Second || cfg.Interval > time.Minute || cfg.Timeout < 100*time.Millisecond || cfg.Timeout > 5*time.Second || cfg.MaxAge < cfg.Interval+cfg.Timeout || cfg.MaxAge > time.Minute {
		return nil, errors.New("vip observation: invalid acquisition bounds")
	}
	if cfg.Kind == "metallb" && cfg.AnnouncerNamespace == "" {
		return nil, errors.New("vip observation: announcer namespace required")
	}
	vips := make([]VIP, 0, len(cfg.Bindings))
	names := map[string]bool{}
	for _, b := range cfg.Bindings {
		if b.Name == "" || names[b.Name] || b.Namespace == "" || b.Service == "" {
			return nil, errors.New("vip observation: invalid or duplicate Service binding")
		}
		if _, err := netip.ParseAddr(b.Address); err != nil {
			return nil, errors.New("vip observation: binding requires an IP address")
		}
		names[b.Name] = true
		vips = append(vips, VIP{Name: b.Name, Address: b.Address})
	}
	p, err := NewServiceProvider(cfg.Kind, vips)
	if err != nil {
		return nil, err
	}
	cfg.Bindings = slices.Clone(cfg.Bindings)
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	p.now = cfg.Now
	return &ServiceObserver{ServiceProvider: p, client: client, cfg: cfg}, nil
}

// Refresh acquires a complete bounded observation. Failed evidence immediately
// replaces a previous owner with unknown; it never extends the old deadline.
func (o *ServiceObserver) Refresh(ctx context.Context) {
	o.refresh.Lock()
	defer o.refresh.Unlock()
	started := o.cfg.Now()
	deadline := started.Add(o.cfg.MaxAge)
	ctx, cancel := context.WithTimeout(ctx, o.cfg.Timeout)
	defer cancel()
	items := make([]OwnerObservation, 0, len(o.cfg.Bindings))
	for _, binding := range o.cfg.Bindings {
		item := OwnerObservation{Name: binding.Name, Address: binding.Address, CheckedAt: started, ExpiresAt: deadline, Reason: "unavailable"}
		if ctx.Err() == nil {
			o.acquire(ctx, binding, &item)
		}
		if ctx.Err() != nil {
			item.Owner, item.Known, item.Reason = "", false, "unavailable"
		}
		items = append(items, item)
	}
	o.mu.Lock()
	o.observation = items
	o.mu.Unlock()
}

// Run polls independently of supervisor launch and cluster writer authority.
func (o *ServiceObserver) Run(ctx context.Context) error {
	o.Refresh(ctx)
	ticker := time.NewTicker(o.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			o.Refresh(ctx)
		}
	}
}

func (o *ServiceObserver) get(ctx context.Context, gvr schema.GroupVersionResource, namespace, name string) (*unstructured.Unstructured, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if namespace == "" {
		return o.client.Resource(gvr).Get(ctx, name, metav1.GetOptions{})
	}
	return o.client.Resource(gvr).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
}

func text(obj *unstructured.Unstructured, path ...string) string {
	value, _, _ := unstructured.NestedString(obj.Object, path...)
	return value
}
func liveObject(obj *unstructured.Unstructured) bool {
	return obj != nil && obj.GetUID() != "" && obj.GetResourceVersion() != "" && obj.GetDeletionTimestamp() == nil
}
func condition(obj *unstructured.Unstructured, kind string) bool {
	values, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, value := range values {
		m, ok := value.(map[string]any)
		if ok && m["type"] == kind && m["status"] == "True" {
			return true
		}
	}
	return false
}
func allocated(service *unstructured.Unstructured, address string) bool {
	if !liveObject(service) || text(service, "spec", "type") != "LoadBalancer" {
		return false
	}
	want, err := netip.ParseAddr(address)
	if err != nil {
		return false
	}
	ingress, _, _ := unstructured.NestedSlice(service.Object, "status", "loadBalancer", "ingress")
	for _, entry := range ingress {
		m, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		ip, _ := m["ip"].(string)
		got, err := netip.ParseAddr(ip)
		if err == nil && got == want {
			return true
		}
	}
	return false
}

func leaseDeadline(lease *unstructured.Unstructured, now time.Time) (time.Time, time.Time, bool) {
	if !liveObject(lease) {
		return time.Time{}, time.Time{}, false
	}
	renewed, err := time.Parse(time.RFC3339Nano, text(lease, "spec", "renewTime"))
	if err != nil || renewed.After(now) {
		return time.Time{}, time.Time{}, false
	}
	duration, ok, _ := unstructured.NestedInt64(lease.Object, "spec", "leaseDurationSeconds")
	if !ok || duration < 1 || duration > 300 {
		return time.Time{}, time.Time{}, false
	}
	expires := renewed.Add(time.Duration(duration) * time.Second)
	return renewed, expires, now.Before(expires)
}

func (o *ServiceObserver) acquire(ctx context.Context, binding ServiceBinding, item *OwnerObservation) {
	service, err := o.get(ctx, serviceGVR, binding.Namespace, binding.Service)
	if err != nil {
		return
	}
	if !allocated(service, binding.Address) {
		item.Reason = "not_allocated"
		return
	}
	item.ServiceUID = string(service.GetUID())
	if o.cfg.Kind == "kube-vip" {
		o.kubeVIP(ctx, binding, service, item)
	} else {
		o.metalLB(ctx, binding, service, item)
	}
	if !item.Known {
		return
	}
	// Fence against Service deletion/replacement or changed status while reading
	// the announcer. A later polling attempt is independent acquisition.
	head, err := o.get(ctx, serviceGVR, binding.Namespace, binding.Service)
	if err != nil || head.GetUID() != service.GetUID() || head.GetResourceVersion() != service.GetResourceVersion() || !allocated(head, binding.Address) {
		item.Owner, item.Known, item.Reason = "", false, "source_changed"
	}
}

func (o *ServiceObserver) kubeVIP(ctx context.Context, binding ServiceBinding, service *unstructured.Unstructured, item *OwnerObservation) {
	item.Reason = "announcer_unknown"
	node := service.GetAnnotations()["kube-vip.io/vipHost"]
	if node == "" {
		return
	}
	lease, err := o.get(ctx, leaseGVR, binding.Namespace, "kubevip-"+binding.Service)
	if err != nil {
		return
	}
	if text(lease, "spec", "holderIdentity") != node || lease.GetCreationTimestamp().Time.Before(service.GetCreationTimestamp().Time) {
		return
	}
	observedNow := o.cfg.Now()
	renewed, expires, ok := leaseDeadline(lease, observedNow)
	expires = observedNow.Add(expires.Sub(observedNow))
	if !ok {
		item.Reason = "stale"
		return
	}
	currentNode, err := o.get(ctx, nodeGVR, "", node)
	if err != nil || !liveObject(currentNode) || !condition(currentNode, "Ready") {
		item.Reason = "announcer_unavailable"
		return
	}
	// A renewal/leadership change during acquisition invalidates this evidence.
	head, err := o.get(ctx, leaseGVR, binding.Namespace, lease.GetName())
	if err != nil || head.GetUID() != lease.GetUID() || head.GetResourceVersion() != lease.GetResourceVersion() {
		item.Reason = "source_changed"
		return
	}
	item.Known, item.Owner, item.Reason = true, node, ""
	item.Evidence = &AnnouncerEvidence{Kind: "kube-vip-service-lease", UID: string(lease.GetUID()), ResourceVersion: lease.GetResourceVersion(), CreatedAt: lease.GetCreationTimestamp().Time, RenewedAt: &renewed}
	if expires.Before(item.ExpiresAt) {
		item.ExpiresAt = expires
	}
}

func (o *ServiceObserver) metalLB(ctx context.Context, binding ServiceBinding, service *unstructured.Unstructured, item *OwnerObservation) {
	item.Reason = "announcer_unknown"
	statuses, err := o.client.Resource(l2GVR).Namespace(o.cfg.AnnouncerNamespace).List(ctx, metav1.ListOptions{Limit: 65})
	if err != nil {
		return
	}
	if len(statuses.Items) > 64 || statuses.GetContinue() != "" {
		item.Reason = "evidence_limit"
		return
	}
	matches := []*unstructured.Unstructured{}
	for i := range statuses.Items {
		status := &statuses.Items[i]
		if text(status, "status", "serviceName") == binding.Service && text(status, "status", "serviceNamespace") == binding.Namespace {
			matches = append(matches, status)
		}
	}
	if len(matches) > 1 {
		item.Reason = "ambiguous"
		return
	}
	if len(matches) != 1 {
		return
	}
	status := matches[0]
	node := text(status, "status", "node")
	interfaces, _, _ := unstructured.NestedSlice(status.Object, "status", "interfaces")
	created, serviceCreated := status.GetCreationTimestamp(), service.GetCreationTimestamp()
	if !liveObject(status) || node == "" || len(interfaces) == 0 || created.Before(&serviceCreated) {
		return
	}
	var podName, podUID string
	for _, ref := range status.GetOwnerReferences() {
		if ref.APIVersion == "v1" && ref.Kind == "Pod" {
			if podName != "" {
				item.Reason = "ambiguous"
				return
			}
			podName, podUID = ref.Name, string(ref.UID)
		}
	}
	if podName == "" || podUID == "" {
		return
	}
	pod, err := o.get(ctx, podGVR, o.cfg.AnnouncerNamespace, podName)
	if err != nil || !liveObject(pod) || string(pod.GetUID()) != podUID || text(pod, "spec", "nodeName") != node || text(pod, "status", "phase") != "Running" || !condition(pod, "Ready") {
		item.Reason = "announcer_unavailable"
		return
	}
	currentNode, err := o.get(ctx, nodeGVR, "", node)
	if err != nil || !liveObject(currentNode) || !condition(currentNode, "Ready") {
		item.Reason = "announcer_unavailable"
		return
	}
	nodeLease, err := o.get(ctx, leaseGVR, "kube-node-lease", node)
	if err != nil || text(nodeLease, "spec", "holderIdentity") != node || !leaseForNode(nodeLease, currentNode) {
		item.Reason = "announcer_unavailable"
		return
	}
	observedNow := o.cfg.Now()
	renewed, expires, ok := leaseDeadline(nodeLease, observedNow)
	expires = observedNow.Add(expires.Sub(observedNow))
	if !ok {
		item.Reason = "stale"
		return
	}
	head, err := o.get(ctx, l2GVR, o.cfg.AnnouncerNamespace, status.GetName())
	if err != nil || head.GetUID() != status.GetUID() || head.GetResourceVersion() != status.GetResourceVersion() {
		item.Reason = "source_changed"
		return
	}
	leaseHead, err := o.get(ctx, leaseGVR, "kube-node-lease", node)
	if err != nil || leaseHead.GetUID() != nodeLease.GetUID() || leaseHead.GetResourceVersion() != nodeLease.GetResourceVersion() {
		item.Reason = "source_changed"
		return
	}
	nodeHead, err := o.get(ctx, nodeGVR, "", node)
	if err != nil || nodeHead.GetUID() != currentNode.GetUID() || nodeHead.GetResourceVersion() != currentNode.GetResourceVersion() {
		item.Reason = "source_changed"
		return
	}
	item.Known, item.Owner, item.Reason = true, node, ""
	item.Evidence = &AnnouncerEvidence{Kind: "metallb-l2-status", UID: string(status.GetUID()), ResourceVersion: status.GetResourceVersion(), CreatedAt: created.Time, LivenessAt: &renewed}
	if expires.Before(item.ExpiresAt) {
		item.ExpiresAt = expires
	}
}

// A node name and holderIdentity survive Node recreation. Kubernetes binds a
// node heartbeat Lease to the exact Node incarnation with its ownerReference.
func leaseForNode(lease, node *unstructured.Unstructured) bool {
	matched := false
	for _, ref := range lease.GetOwnerReferences() {
		if ref.Kind != "Node" {
			continue
		}
		if matched || ref.APIVersion != "v1" || ref.Name != node.GetName() || ref.UID != node.GetUID() {
			return false
		}
		matched = true
	}
	return matched
}
