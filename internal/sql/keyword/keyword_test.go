package keyword

import "testing"

func TestLookup(t *testing.T) {
	cases := map[string]Category{"select": Reserved, "like": TypeFuncName, "integer": ColName, "key": Unreserved}
	for w, want := range cases {
		if got, ok := Lookup(w); !ok || got != want {
			t.Errorf("Lookup(%q) = %d, %v", w, got, ok)
		}
	}
	if _, ok := Lookup("users"); ok {
		t.Error("users is not a keyword")
	}
	if _, ok := Lookup("SELECT"); ok {
		t.Error("Lookup takes lower-case words")
	}
}
