package zonecache

import (
	"reflect"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

func TestZeroObjectSet(t *testing.T) {
	var s ObjectSet
	if s.Len() != 0 || s.Count(KindDNSZone) != 0 || s.List(KindDNSZone) != nil || s.Kinds() != nil ||
		s.Zones() != nil || s.RecordSetsForZone("x") != nil || len(s.ZoneRefs()) != 0 || s.Lists() != nil ||
		s.ResourceVersion(KindDNSZone) != "" || s.Select(KindDNSZone, map[string]string{"a": "b"}) != nil {
		t.Fatal("zero ObjectSet is not empty")
	}
	if _, ok := s.Get(store.Key{Kind: KindDNSZone, Name: "x"}); ok {
		t.Fatal("zero ObjectSet Get found something")
	}
	if _, ok := s.Zone("x"); ok {
		t.Fatal("zero ObjectSet Zone found something")
	}
}

func TestSelect(t *testing.T) {
	c := newCache(t, newFakeStore(), Options{})
	objs := []store.Object{
		rrset("b", "web", "home", map[string]string{"team": "web", "env": "prod"}),
		rrset("a", "api", "home", map[string]string{"team": "web", "env": "dev"}),
		rrset("a", "db", "lab", map[string]string{"team": "data", "env": "prod"}),
		rrset("a", "bare", "lab", nil),
	}
	set := ObjectSet{idx: c.indexOf(map[store.Kind][]store.Object{KindDNSRecordSet: objs}, nil, map[store.Kind]string{KindDNSRecordSet: "9"})}
	for _, tc := range []struct {
		name string
		sel  map[string]string
		want []string
	}{
		{"empty selects all, sorted", nil, []string{"a/api", "a/bare", "a/db", "b/web"}},
		{"one pair", map[string]string{"team": "web"}, []string{"a/api", "b/web"}},
		{"two pairs", map[string]string{"team": "web", "env": "prod"}, []string{"b/web"}},
		{"no match on value", map[string]string{"team": "ops"}, nil},
		{"no match on key", map[string]string{"owner": "x"}, nil},
		{"pairs that never co-occur", map[string]string{"team": "data", "env": "dev"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := set.Select(KindDNSRecordSet, tc.sel)
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if n := names(got); !reflect.DeepEqual(n, tc.want) {
				t.Fatalf("Select(%v) = %v, want %v", tc.sel, n, tc.want)
			}
		})
	}
	if got := names(set.List(KindDNSRecordSet)); !reflect.DeepEqual(got, []string{"a/api", "a/bare", "a/db", "b/web"}) {
		t.Fatalf("List order %v", got)
	}
	if rs := set.RecordSetsForZone("lab"); len(rs) != 2 || rs[0].Name != "bare" || rs[1].Name != "db" {
		t.Fatalf("RecordSetsForZone(lab) = %v", rs)
	}
	if set.ResourceVersion(KindDNSRecordSet) != "9" {
		t.Fatal("resourceVersion not carried")
	}
	// Returned slices are clipped: appending never writes into the index.
	l := set.List(KindDNSRecordSet)
	_ = append(l, zone("x", "x."))
	if len(set.List(KindDNSRecordSet)) != 4 || cap(l) != len(l) {
		t.Fatal("List slice not clipped")
	}
}

func TestNextExpiry(t *testing.T) {
	c := newCache(t, newFakeStore(), Options{})
	base := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	withExpiry := func(name string, at ...time.Time) store.Object {
		rs := rrset("apps", name, "home", nil)
		rs.Spec.Endpoints = nil
		for _, a := range at {
			rs.Spec.Endpoints = append(rs.Spec.Endpoints, v1alpha1.Endpoint{DNSName: name + ".home.example.", RecordType: "A",
				Targets: []string{"10.0.0.1"}, Kudurru: &v1alpha1.EndpointExt{ExpiresAt: &metav1.Time{Time: a}}})
		}
		return rs
	}
	objs := []store.Object{
		withExpiry("a", base.Add(time.Hour), base.Add(-time.Hour)),
		withExpiry("b", base.Add(10*time.Minute), base.Add(time.Hour)),
		rrset("apps", "plain", "home", nil),
	}
	set := ObjectSet{idx: c.indexOf(map[store.Kind][]store.Object{KindDNSRecordSet: objs}, nil, nil)}
	for _, tc := range []struct {
		now  time.Time
		want time.Time
	}{
		{base.Add(-2 * time.Hour), base.Add(-time.Hour)},
		{base, base.Add(10 * time.Minute)},
		{base.Add(10 * time.Minute), base.Add(time.Hour)},
		{base.Add(time.Hour), time.Time{}},
	} {
		if got := set.nextExpiry(tc.now); !got.Equal(tc.want) {
			t.Errorf("nextExpiry(%s) = %s, want %s", tc.now, got, tc.want)
		}
	}
}

func TestChangedFlags(t *testing.T) {
	s := ObjectSet{changed: map[store.Kind]bool{KindDNSZone: true}}
	if !s.Changed(KindDNSZone) || s.Changed(KindPolicy) || s.ListsChanged() {
		t.Fatal("changed flags")
	}
	s.allChanged = true
	if !s.Changed(KindPolicy) || !s.ListsChanged() {
		t.Fatal("allChanged must imply every kind and the lists")
	}
}
