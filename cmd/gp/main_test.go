package main

import (
	"strings"
	"testing"
)

func TestVersion(t *testing.T) {
	fn := "cmd/gp.Version"
	if got := Version; !strings.HasPrefix(got, "v") {
		t.Fatalf("%s: got %q, want prefix 'v'", fn, got)
	}
}
