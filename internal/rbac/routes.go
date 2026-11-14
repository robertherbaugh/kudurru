package rbac

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

// Has reports whether permissions.yaml declares verb on resource.
func (p *Permissions) Has(resource, verb string) bool {
	return slices.Contains(p.Resources[resource], verb)
}

// ResourceNames lists the declared resources, sorted.
func (p *Permissions) ResourceNames() []string {
	out := make([]string, 0, len(p.Resources))
	for r := range p.Resources {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// Route is one HTTP route and the permission it declares (decision D31:
// every API route declares {resource, verb, scope}). Public routes (login,
// status, version, the OpenAPI document) declare Public instead and are
// served to anonymous callers; Authenticated routes need a principal but
// no permission (GET /auth/me).
type Route struct {
	Method   string
	Path     string
	Resource string
	Verb     string
	// Resources lists every resource a route may check when its resource
	// depends on a path parameter (/objects/{kind}); Resource is then "".
	Resources     []string
	Public        bool
	Authenticated bool
}

// LintRoutes is the route-permission lint: it returns one error for every
// route that declares nothing, or names a resource or verb that
// permissions.yaml does not define. The API's tests run it over the
// registered route table so a new route without a declaration fails the
// build.
func LintRoutes(p *Permissions, routes []Route) []error {
	var errs []error
	seen := map[string]bool{}
	for _, r := range routes {
		id := r.Method + " " + r.Path
		if seen[id] {
			errs = append(errs, fmt.Errorf("rbac: route %s is declared twice", id))
		}
		seen[id] = true
		if r.Public || r.Authenticated {
			if r.Resource != "" || len(r.Resources) > 0 {
				errs = append(errs, fmt.Errorf("rbac: route %s is public or authenticated-only but also names a resource", id))
			}
			continue
		}
		resources := r.Resources
		if r.Resource != "" {
			resources = append([]string{r.Resource}, resources...)
		}
		if len(resources) == 0 || r.Verb == "" {
			errs = append(errs, fmt.Errorf("rbac: route %s declares no permission", id))
			continue
		}
		for _, res := range resources {
			if !p.Has(res, r.Verb) {
				errs = append(errs, fmt.Errorf("rbac: route %s declares %s.%s, which permissions.yaml does not define", id, res, r.Verb))
			}
		}
	}
	return errs
}

// LintVerbUse is the reverse of LintRoutes (D393): every verb that
// permissions.yaml declares must be checked somewhere, or be listed under
// reserved with the release that is to use it. used maps "<resource>.<verb>"
// to where it is checked (a route, or a handler's own check); the caller
// builds it from the mounted route table and the handler sources. A
// reservation for a verb that is in use is stale and is an error too, so
// the reserved list cannot hide a permission that already guards a route.
func LintVerbUse(p *Permissions, used map[string][]string) []error {
	var errs []error
	for _, res := range p.ResourceNames() {
		for _, verb := range p.Resources[res] {
			perm := res + "." + verb
			_, reserved := p.Reserved[perm]
			switch where := used[perm]; {
			case len(where) == 0 && !reserved:
				errs = append(errs, fmt.Errorf("rbac: %s is declared but no route or handler checks it; use it or reserve it with its target release", perm))
			case len(where) > 0 && reserved:
				errs = append(errs, fmt.Errorf("rbac: %s is reserved but is checked by %s; remove the reservation", perm, where[0]))
			}
		}
	}
	for perm := range used {
		res, verb, _ := strings.Cut(perm, ".")
		if !p.Has(res, verb) {
			errs = append(errs, fmt.Errorf("rbac: %s is checked by %s but permissions.yaml does not declare it", perm, used[perm][0]))
		}
	}
	sort.Slice(errs, func(i, j int) bool { return errs[i].Error() < errs[j].Error() })
	return errs
}
