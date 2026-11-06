package main

import (
	"context"
	"testing"
)

func TestUnpreparedRunningGuestIsNotStopped(t *testing.T) {
	a, c, f := testController(t)
	f.failHelper = true
	if _, err := activate(t, a, context.Background(), c, true); err == nil {
		t.Fatal("missing guest helper accepted")
	}
	if countCalls(f, "limactl", "stop") != 0 || countCalls(f, "limactl", "edit") != 0 || countCalls(f, "limactl", "start") != 0 {
		t.Fatal("unprepared VM was disrupted")
	}
	for _, call := range f.calls {
		if call.Name == "resolver" {
			t.Fatal("resolver was run for an unprepared VM")
		}
	}
}
