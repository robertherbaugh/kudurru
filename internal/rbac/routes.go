package rbac

import (
	"fmt"
	"slices"
	"sort"
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
