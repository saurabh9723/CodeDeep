package tools

// CloneWithout copies the registry while omitting the named tools.
func (r *Registry) CloneWithout(exclude ...string) *Registry {
	skip := make(map[string]bool, len(exclude))
	for _, name := range exclude {
		skip[name] = true
	}
	clone := NewRegistry()
	for _, spec := range r.List() {
		if skip[spec.Name] {
			continue
		}
		clone.Register(spec)
	}
	return clone
}
