package deploy_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/ee/edge/vip"
)

func TestVIPServiceAnnotationContract(t *testing.T) {
	for _, provider := range []string{"kube-vip", "metallb"} {
		for _, split := range []bool{false, true} {
			for _, mode := range []string{"direct", "sidecar", "deployment", "daemonset"} {
				t.Run(fmt.Sprintf("%s/split=%t/%s", provider, split, mode), func(t *testing.T) {
					addressKey := "metallb.io/loadBalancerIPs"
					if provider == "kube-vip" {
						addressKey = "kube-vip.io/loadbalancerIPs"
					}
					values := fmt.Sprintf("vip: {provider: %s, address: '2001:db8::53', metallbPool: lan}\nservice: {dns: {splitProtocols: %t, ipFamilies: [IPv6], annotations: {example.com/operator: retained, %s: overridden}}}\n", provider, split, addressKey)
					if mode != "direct" {
						values += fmt.Sprintf("edge: {enabled: true, mode: %s}\n", mode)
					}
					out, err := renderChart(t, values)
					if err != nil {
						t.Fatal(err)
					}
					r := newRendered(t, out)
					r.checkGeneric(t)
					p, err := vip.NewServiceProvider(provider, []vip.VIP{{Name: "dns", Address: "2001:db8::53"}})
					if err != nil {
						t.Fatal(err)
					}
					options := vip.ServiceAnnotationOptions{Pool: "lan"}
					names := []string{"kudurru-dns"}
					if split {
						options.SharingKey = "kudurru"
						names = []string{"kudurru-dns-udp", "kudurru-dns-tcp"}
					}
					want := p.AnnotationsWithOptions("dns", options)
					want["example.com/operator"] = "retained"
					for _, name := range names {
						svc := r.object(t, "Service", name)
						got := mapOf(dig(svc, "metadata", "annotations"))
						if len(got) != len(want) {
							t.Fatalf("%s annotations: %v, want %v", name, got, want)
						}
						for key, value := range want {
							if got[key] != value {
								t.Errorf("%s annotation %s: %v, want %s", name, key, got[key], value)
							}
						}
						if dig(svc, "spec", "type") != "LoadBalancer" {
							t.Fatal("provider Service lost LoadBalancer type")
						}
					}
				})
			}
		}
	}
}

func TestVIPServiceProviderRequiresLoadBalancer(t *testing.T) {
	for _, provider := range []string{"kube-vip", "metallb"} {
		for _, kind := range []string{"ClusterIP", "NodePort"} {
			t.Run(provider+"/"+kind, func(t *testing.T) {
				_, err := renderChart(t, fmt.Sprintf("vip: {provider: %s, address: 192.0.2.53}\nservice: {dns: {type: %s}}", provider, kind))
				if err == nil || !strings.Contains(err.Error(), "requires service.dns.type=LoadBalancer") {
					t.Fatalf("invalid topology was not refused: %v", err)
				}
			})
		}
	}
}
