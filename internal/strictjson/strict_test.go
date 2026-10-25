package strictjson

import (
	"strings"
	"testing"
)

func TestShapeBoundsAndAmbiguityApplyBeforeTypedAssignment(t *testing.T) {
	type record struct {
		Revision int `json:"revision"`
	}
	for _, bad := range []string{`{"revision":1,"revision":2}`, `{"Revision":1}`, `{"revision":1} {}`, strings.Repeat("[", 65) + "0" + strings.Repeat("]", 65)} {
		target := record{Revision: 99}
		if err := Decode([]byte(bad), &target, 1000); err == nil || target.Revision != 99 {
			t.Fatal("ambiguous shape reached assignment", err)
		}
	}
	if err := Check([]byte(`[0,1,2]`), 3); err == nil {
		t.Fatal("node budget ignored")
	}
	if err := Check([]byte(`[0,1,2]`), 4); err != nil {
		t.Fatal("exact node budget rejected", err)
	}
	if err := Check([]byte(`{"ID":1,"id":2}`), 3); err != nil {
		t.Fatal("generic map keys folded", err)
	}
}
