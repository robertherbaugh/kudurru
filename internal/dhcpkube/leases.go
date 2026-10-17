// Package dhcpkube runs DHCPv4 on the Kubernetes store: scopes are
// DHCPScope objects (kudurru.io), leases are DHCPLease objects
// (dhcp.kudurru.io), and one replica, the holder of the kudurru-dhcp Lease,
// answers DHCP through a relay to a LoadBalancer address. Every replica keeps
// a local working copy of the leases for the API and the lease DNS records
// (docs/architecture/dhcp.md, D180).
package dhcpkube

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/netip"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dhcpv1 "github.com/robertherbaugh/kudurru/api/dhcp/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dhcp"
)

// LabelActive marks the pod that serves DHCP; the chart's DHCP Service
// selects it, so the LoadBalancer sends relayed packets only there.
const LabelActive = "kudurru.io/dhcp-active"

// Errors of a LeaseAPI, matching the apiserver's.
var (
	ErrExists   = errors.New("dhcpkube: lease object already exists")
	ErrConflict = errors.New("dhcpkube: lease object changed")
	ErrNotFound = errors.New("dhcpkube: lease object not found")
)

// LeaseAPI reads and writes DHCPLease objects in one namespace. Create and
// Update set the new resourceVersion on obj. Update and Delete use the
// given resourceVersion as a precondition. Watch delivers events after rv
// in order; an Error event or a closed channel requires a new List.
type LeaseAPI interface {
	List(ctx context.Context) (items []dhcpv1.DHCPLease, rv string, err error)
	Watch(ctx context.Context, rv string) (watch.Interface, error)
	Get(ctx context.Context, name string) (*dhcpv1.DHCPLease, error)
	Create(ctx context.Context, obj *dhcpv1.DHCPLease) error
	Update(ctx context.Context, obj *dhcpv1.DHCPLease) error
	Delete(ctx context.Context, name, rv string) error
}

// PodLabeler sets and clears LabelActive. ActivePods lists the pods that
// carry it.
type PodLabeler interface {
	SetActive(ctx context.Context, pod string, active bool) error
	ActivePods(ctx context.Context) ([]string, error)
}

// ObjectName is the DHCPLease name of an address: "<scope>.<a>.<b>.<c>.<d>".
// Kubernetes refuses a second object with the same name, so an address can
// have only one owner even while two replicas briefly both serve.
func ObjectName(scope string, address netip.Addr) string {
	return scope + "." + address.String()
}

// leaseObject converts a lease row into its DHCPLease object.
func leaseObject(namespace, scope string, lease dhcp.ExternalLease) *dhcpv1.DHCPLease {
	r := lease.Record
	obj := &dhcpv1.DHCPLease{
		TypeMeta: metav1.TypeMeta{APIVersion: dhcpv1.GroupVersion.String(), Kind: "DHCPLease"},
		ObjectMeta: metav1.ObjectMeta{Name: ObjectName(scope, r.Address), Namespace: namespace,
			Labels: map[string]string{dhcpv1.LabelScope: scope, "app.kubernetes.io/managed-by": "kudurru"}},
		Spec: dhcpv1.DHCPLeaseSpec{Scope: scope, Address: r.Address.String(),
			Identity: dhcpv1.DHCPLeaseIdentity{Kind: r.Identity.Kind, Value: r.Identity.Value},
			MAC:      r.MAC, Hostname: r.Hostname, ExpiresAt: metav1.NewTime(r.ExpiresAt.UTC()), State: r.State},
	}
	if c := lease.Claim; c.Ticket > 0 {
		obj.Spec.DNSClaim = &dhcpv1.DHCPLeaseClaim{Name: c.Name, Epoch: int64(c.Epoch), Ticket: int64(c.Ticket), Ordinal: int64(c.Ordinal)} // #nosec G115 -- validated to fit int64 by the lease store
	}
	return obj
}

// leaseFromObject converts a DHCPLease object into its lease row.
func leaseFromObject(obj *dhcpv1.DHCPLease) (string, dhcp.ExternalLease, error) {
	address, err := netip.ParseAddr(obj.Spec.Address)
	if err != nil || !address.Is4() {
		return "", dhcp.ExternalLease{}, fmt.Errorf("dhcpkube: lease %s: invalid address %q", obj.Name, obj.Spec.Address)
	}
	if obj.Name != ObjectName(obj.Spec.Scope, address) {
		return "", dhcp.ExternalLease{}, fmt.Errorf("dhcpkube: lease %s: name does not match scope and address", obj.Name)
	}
	if obj.Spec.Identity.Kind == "client-id" {
		if _, err := base64.StdEncoding.Strict().DecodeString(obj.Spec.Identity.Value); err != nil {
			return "", dhcp.ExternalLease{}, fmt.Errorf("dhcpkube: lease %s: invalid client identity", obj.Name)
		}
	}
	lease := dhcp.ExternalLease{Record: dhcp.LeaseRecord{Address: address,
		Identity: dhcp.LeaseIdentity{Kind: obj.Spec.Identity.Kind, Value: obj.Spec.Identity.Value},
		MAC:      obj.Spec.MAC, Hostname: obj.Spec.Hostname, ExpiresAt: obj.Spec.ExpiresAt.UTC().Truncate(time.Second), State: obj.Spec.State}}
	if c := obj.Spec.DNSClaim; c != nil {
		if c.Epoch < 0 || c.Ticket < 1 || c.Ordinal < 0 {
			return "", dhcp.ExternalLease{}, fmt.Errorf("dhcpkube: lease %s: invalid DNS claim", obj.Name)
		}
		lease.Claim = dhcp.DNSClaim{Name: c.Name, Epoch: uint64(c.Epoch), Ticket: uint64(c.Ticket), Ordinal: uint64(c.Ordinal)}
	}
	return obj.Spec.Scope, lease, nil
}

// ClientLeases implements LeaseAPI with a controller-runtime client.
type ClientLeases struct {
	Client    client.WithWatch
	Namespace string
}

func clientError(err error) error {
	switch {
	case err == nil:
		return nil
	case apierrors.IsAlreadyExists(err):
		return fmt.Errorf("%w: %v", ErrExists, err)
	case apierrors.IsConflict(err):
		return fmt.Errorf("%w: %v", ErrConflict, err)
	case apierrors.IsNotFound(err):
		return fmt.Errorf("%w: %v", ErrNotFound, err)
	}
	return err
}

// List implements LeaseAPI with a consistent read from the apiserver.
func (c *ClientLeases) List(ctx context.Context) ([]dhcpv1.DHCPLease, string, error) {
	var list dhcpv1.DHCPLeaseList
	if err := c.Client.List(ctx, &list, client.InNamespace(c.Namespace)); err != nil {
		return nil, "", err
	}
	return list.Items, list.ResourceVersion, nil
}

// Watch implements LeaseAPI.
func (c *ClientLeases) Watch(ctx context.Context, rv string) (watch.Interface, error) {
	var list dhcpv1.DHCPLeaseList
	return c.Client.Watch(ctx, &list, client.InNamespace(c.Namespace),
		&client.ListOptions{Raw: &metav1.ListOptions{ResourceVersion: rv, AllowWatchBookmarks: true}})
}

// Get implements LeaseAPI.
func (c *ClientLeases) Get(ctx context.Context, name string) (*dhcpv1.DHCPLease, error) {
	var obj dhcpv1.DHCPLease
	if err := c.Client.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: name}, &obj); err != nil {
		return nil, clientError(err)
	}
	return &obj, nil
}

// Create implements LeaseAPI.
func (c *ClientLeases) Create(ctx context.Context, obj *dhcpv1.DHCPLease) error {
	obj.Namespace = c.Namespace
	return clientError(c.Client.Create(ctx, obj))
}

// Update implements LeaseAPI.
func (c *ClientLeases) Update(ctx context.Context, obj *dhcpv1.DHCPLease) error {
	obj.Namespace = c.Namespace
	return clientError(c.Client.Update(ctx, obj))
}

// Delete implements LeaseAPI.
func (c *ClientLeases) Delete(ctx context.Context, name, rv string) error {
	obj := &dhcpv1.DHCPLease{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: c.Namespace}}
	return clientError(c.Client.Delete(ctx, obj, client.Preconditions{ResourceVersion: &rv}))
}

// ClientPods implements PodLabeler with a controller-runtime client.
type ClientPods struct {
	Client    client.Client
	Namespace string
	// Selector restricts ActivePods to this release's pods.
	Selector map[string]string
}

// SetActive implements PodLabeler with a JSON merge patch of one label.
func (p *ClientPods) SetActive(ctx context.Context, pod string, active bool) error {
	value := "null"
	if active {
		value = `"true"`
	}
	patch := []byte(`{"metadata":{"labels":{"` + LabelActive + `":` + value + `}}}`)
	obj := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: pod, Namespace: p.Namespace}}
	err := p.Client.Patch(ctx, obj, client.RawPatch(types.MergePatchType, patch))
	if apierrors.IsNotFound(err) && !active {
		return nil
	}
	return err
}

// ActivePods implements PodLabeler.
func (p *ClientPods) ActivePods(ctx context.Context) ([]string, error) {
	selector := client.MatchingLabels{LabelActive: "true"}
	for k, v := range p.Selector {
		selector[k] = v
	}
	var list metav1.PartialObjectMetadataList
	list.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("PodList"))
	if err := p.Client.List(ctx, &list, client.InNamespace(p.Namespace), selector); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(list.Items))
	for _, item := range list.Items {
		names = append(names, item.Name)
	}
	return names, nil
}
