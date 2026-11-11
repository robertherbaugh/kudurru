package host

import "github.com/robertherbaugh/kudurru/ee/integration"

// Roles names what one provider incarnation does for the host: publish
// records, observe a source, receive events. A provider whose Go type serves
// every capability of its protocol, while the peer it negotiated with offers
// only some of them, reports the negotiated subset.
type Roles struct {
	Publisher, Observer, Hook bool
}

// RoleProvider is optional. The host consults it only after a successful
// Check and before each use of a role, so the answer must be fixed for the
// incarnation, perform no I/O and grant no authority. A role is valid only
// when the Go type also implements its interface (Publisher, Observer,
// DeliveryHook); a declared role the type cannot serve is ErrInvalid.
// Providers without this interface keep the type-assertion default, under
// which every interface a type implements is a role.
type RoleProvider interface {
	Roles() (Roles, error)
}

// providerRoles is the single place the host decides a provider's roles.
func providerRoles(p integration.Integration) (Roles, error) {
	_, publisher := p.(Publisher)
	_, observer := p.(Observer)
	_, hook := p.(DeliveryHook)
	negotiated, ok := p.(RoleProvider)
	if !ok {
		return Roles{Publisher: publisher, Observer: observer, Hook: hook}, nil
	}
	r, err := negotiated.Roles()
	if err != nil {
		return Roles{}, err
	}
	if r.Publisher && !publisher || r.Observer && !observer || r.Hook && !hook {
		return Roles{}, ErrInvalid
	}
	return r, nil
}

func negotiatedRoles(p integration.Integration) bool {
	_, ok := p.(RoleProvider)
	return ok
}
