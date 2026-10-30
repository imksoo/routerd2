package resourceprogram

import (
	"fmt"
	"slices"
)

// RemoveOwned removes an owner and all resources transitively owned by it.
// It rejects dangling non-ownership dependencies rather than silently deleting
// unrelated operator declarations. It does not mutate the input program.
func RemoveOwned(source Program, id string) (Program, []string, error) {
	present := false
	removed := map[string]bool{id: true}
	for _, r := range source.Resources {
		if r.ID == id {
			present = true
		}
	}
	if !present {
		return Program{}, nil, fmt.Errorf("resource %q does not exist", id)
	}
	for changed := true; changed; {
		changed = false
		for _, r := range source.Resources {
			if !removed[r.ID] && r.Owner != "" && removed[r.Owner] {
				removed[r.ID] = true
				changed = true
			}
		}
	}
	for _, r := range source.Resources {
		if removed[r.ID] {
			continue
		}
		for _, dep := range r.DependsOn {
			if removed[dep] {
				return Program{}, nil, fmt.Errorf("resource %q depends on removed resource %q", r.ID, dep)
			}
		}
	}
	out := Program{SchemaVersion: source.SchemaVersion, Retention: source.Retention, Resources: make([]Resource, 0, len(source.Resources)-len(removed))}
	ids := make([]string, 0, len(removed))
	for _, r := range source.Resources {
		if removed[r.ID] {
			ids = append(ids, r.ID)
			continue
		}
		out.Resources = append(out.Resources, r)
	}
	slices.Sort(ids)
	return out, ids, nil
}
