package servingbuild

import (
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingdns"
)

// DNSKeys returns only the original complete DNS publication's immutable ring.
// Held read requests retain it even when successor publication is current. This
// does not confer current mutation authority or consult a coordinator/store.
func DNSKeys(req *pipeline.Request) (*dnsserver.StaticKeyring, error) {
	owner, err := servingdns.Component[*RuntimeOwner](req, "runtime")
	if err != nil {
		return nil, err
	}
	if owner.keys == nil {
		return nil, serving.ErrDependency
	}
	return owner.keys, nil
}
