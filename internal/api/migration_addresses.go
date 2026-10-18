package api

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	internal "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/importers"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// Generated records are additive and scoped to the run, zone and address set.
// Existing records are never adopted or overwritten implicitly.
func (s *Server) migrationAddressRecordSet(ctx context.Context, run *internal.MigrationRun, z *v1alpha1.DNSZone, ips []string) (*v1alpha1.DNSRecordSet, error) {
	if z.Spec.RecordSetSelector != nil {
		return nil, errInvalid("Zone %s has a record-set selector; use explicit nameserver DNS names", z.Name)
	}
	_, endpoints, err := importers.Authority(z.Spec.ZoneName, nil, ips)
	if err != nil {
		return nil, errInvalid("%s", err)
	}
	hash := sha256.Sum256([]byte(run.Name + "/" + z.Name + "/" + strings.Join(ips, ",")))
	rs := &v1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("migration-ns-%x", hash[:12]), Namespace: s.opts.DefaultNamespace, Annotations: map[string]string{migrationRunAnnotation: run.Name}},
		Spec:       v1alpha1.DNSRecordSetSpec{ZoneRef: v1alpha1.ObjectRef{Name: z.Name}, Endpoints: endpoints},
	}
	var existing v1alpha1.DNSRecordSet
	err = s.opts.Store.Get(ctx, store.Key{Kind: "DNSRecordSet", Namespace: rs.Namespace, Name: rs.Name}, &existing)
	if err == nil {
		if existing.Annotations[migrationRunAnnotation] != run.Name || !reflect.DeepEqual(existing.Spec, rs.Spec) || store.ManagedBy(&existing) != store.ManagerAPI {
			return nil, errConflict("Generated nameserver record set was changed", "Use explicit DNS names or review the address records")
		}
		rs.ResourceVersion = existing.ResourceVersion
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, storeError(err)
	}
	sets, err := s.zoneRecordSets(ctx, z.Name)
	if err != nil {
		return nil, err
	}
	for _, set := range sets {
		for _, endpoint := range set.Spec.Endpoints {
			for _, target := range endpoints {
				if !strings.EqualFold(strings.TrimSuffix(endpoint.DNSName, "."), strings.TrimSuffix(target.DNSName, ".")) {
					continue
				}
				// A previous cutover can already serve the identical mapping.
				// Refuse all other data (including aliases, views and disabled records).
				if set.Annotations[migrationRunAnnotation] != run.Name || !reflect.DeepEqual(endpoint, target) {
					return nil, errConflict("Generated nameserver conflicts with existing records: "+target.DNSName, "Use explicit DNS names or review the address records")
				}
			}
		}
	}
	k, _ := kindByKind("DNSRecordSet")
	body, err := objectMap(k, rs)
	if err == nil {
		err = s.authorizeWrite(ctx, k, writeRequest{kind: k, body: body})
	}
	return rs, err
}

func (s *Server) writeMigrationAddresses(ctx context.Context, run *internal.MigrationRun, z *v1alpha1.DNSZone, ips []string, reviewed *v1alpha1.DNSRecordSet) (ApplyResult, error) {
	item := ApplyResult{Kind: "DNSRecordSet", Namespace: reviewed.Namespace, Name: reviewed.Name, Result: ResultError}
	rs, err := s.migrationAddressRecordSet(ctx, run, z, ips)
	if err == nil && rs.ResourceVersion != reviewed.ResourceVersion {
		err = errPrecondition(reviewed.ResourceVersion, rs.ResourceVersion)
	}
	if err == nil {
		k, _ := kindByKind("DNSRecordSet")
		body, e := objectMap(k, rs)
		err = e
		if err == nil {
			wr, e := s.writeObject(ctx, writeRequest{kind: k, body: body, ifMatch: reviewed.ResourceVersion, createOnly: reviewed.ResourceVersion == ""})
			err = e
			if err == nil {
				item.Result = wr.result
				item.ResourceVersion = wr.obj.GetResourceVersion()
			}
		}
	}
	if err != nil {
		item.Error = err.Error()
	}
	return item, err
}
