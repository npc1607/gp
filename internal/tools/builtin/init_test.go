package builtin

import (
	"testing"
)

func TestNewDefaultRegistry(t *testing.T) {
	fn := "NewDefaultRegistry"
	reg, err := NewDefaultRegistry()
	if err != nil {
		t.Fatalf("%s(): got unexpected error %v, want nil", fn, err)
	}

	expectedTools := []string{"read", "write", "edit", "bash"}
	for _, name := range expectedTools {
		if _, ok := reg.Get(name); !ok {
			t.Errorf("%s(): got tool %q registered=%v, want true", fn, name, ok)
		}
	}

	if got, want := len(reg.List()), len(expectedTools); got != want {
		t.Errorf("%s(): got total registered %d tools, want %d", fn, got, want)
	}
}
