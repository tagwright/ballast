// SPDX-License-Identifier: GPL-3.0-or-later

package runtime

import (
	"testing"

	"github.com/docker/docker/api/types/events"
)

// TestMapEventActionDestroyAndRemoveBothMapToEventDestroy proves both
// Docker's own container-removal action ("destroy") and Podman's compat-API
// equivalent ("remove") normalize to EventDestroy, which
// internal/daemon/watch.go's handleEvent treats identically (unregister the
// service alongside EventDie).
//
// This is a regression test for a real bug found running the Podman adapter
// against a live Podman 5.8 socket for the first time: mapEventAction only
// recognized events.ActionDestroy, so a container removed under Podman
// while already stopped (no accompanying "die" event to unregister it via
// the other branch) left its scheduled job registered forever, since
// Podman's compat API never emits "destroy" for a container, only
// "remove". Confirmed directly against the live socket (curl against the
// raw /v1.41/events endpoint) before this fix, not merely inferred from
// docs.
func TestMapEventActionDestroyAndRemoveBothMapToEventDestroy(t *testing.T) {
	for _, action := range []events.Action{events.ActionDestroy, events.ActionRemove} {
		et, ok := mapEventAction(action)
		if !ok {
			t.Fatalf("mapEventAction(%q): got ok=false, want true", action)
		}
		if et != EventDestroy {
			t.Fatalf("mapEventAction(%q): got %q, want %q", action, et, EventDestroy)
		}
	}
}

// TestMapEventActionKnownActions locks in the rest of the mapping so a
// future edit that silently drops one of Ballast's four acted-on lifecycle
// events (start, stop, die, and destroy/remove above) fails a test instead
// of shipping quietly.
func TestMapEventActionKnownActions(t *testing.T) {
	cases := []struct {
		action events.Action
		want   EventType
	}{
		{events.ActionStart, EventStart},
		{events.ActionStop, EventStop},
		{events.ActionDie, EventDie},
	}
	for _, c := range cases {
		et, ok := mapEventAction(c.action)
		if !ok || et != c.want {
			t.Fatalf("mapEventAction(%q) = (%q, %v), want (%q, true)", c.action, et, ok, c.want)
		}
	}
}

// TestMapEventActionUnknownActionIsSkipped proves an action Ballast does
// not act on (health checks, exec, resize, and the like) reports ok=false
// rather than a zero-value EventType the caller might mistake for
// meaningful.
func TestMapEventActionUnknownActionIsSkipped(t *testing.T) {
	if _, ok := mapEventAction(events.ActionHealthStatus); ok {
		t.Fatalf("mapEventAction(health_status): got ok=true, want false")
	}
}
