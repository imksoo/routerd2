//go:build linux

package supervisor

import "testing"

func TestRoleCatalogRequiresEveryInitialAndDynamicRoleAndCopiesCallerMap(t *testing.T) {
	base := Options{Platform: "host", ReleaseID: "test", Executables: map[string]string{"network": "/test/network", "health": "/test/protocols"}, Modules: []Spec{{Name: "network", Role: "network"}}, DynamicRoles: []string{"health"}}
	got, err := normalize(base)
	if err != nil {
		t.Fatal(err)
	}
	base.Executables["network"] = "/changed"
	if got.Executables["network"] != "/test/network" {
		t.Fatal("caller can change normalized role binding")
	}
	for _, role := range []string{"network", "health"} {
		bad := got
		bad.Executables = map[string]string{}
		for k, v := range got.Executables {
			if k != role {
				bad.Executables[k] = v
			}
		}
		if _, err := normalize(bad); err == nil {
			t.Fatal("undeclared executable role accepted", role)
		}
	}
}
