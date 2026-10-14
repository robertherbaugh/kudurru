package providerregistry

// ClusterID returns the cluster bound to this validated registry, or empty for nil.
func (r *Registry) ClusterID() string {
	if r == nil {
		return ""
	}
	return r.root.ClusterID
}
