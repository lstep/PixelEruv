package worldsim

import (
	"log/slog"
	"testing"
)

// TestIsPositionBlocked_SubTileWall probes whether a wall zone thinner than a
// tile still blocks the player. Zone Contains is continuous-space, but
// isPositionBlocked samples 5 points with 0.3 spacing around the feet — so
// the player is blocked when any sample point lands inside the wall, not
// only when the feet center is inside it.
func TestIsPositionBlocked_SubTileWall(t *testing.T) {
	// Wall 0.2 tiles thick at feet-Y [5.0, 5.2], spanning X.
	zones := []*Zone{{ID: "thin", Shape: ShapeRect, X: 0, Y: 5, W: 20, H: 0.2}}
	s := &Simulator{
		zoneReg: NewZoneRegistry(zones, 20, 20),
		extMgr:  NewExtensionManager(slog.Default()),
	}
	if err := s.extMgr.Register([]byte(`{"extension_id":"ext-walls","heartbeat_interval_s":10}`)); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := s.extMgr.RegisterTriggers([]byte(`{
		"extension_id": "ext-walls",
		"gate_triggers": [{"zone_id": "thin", "behavior": "block"}]
	}`)); err != nil {
		t.Fatalf("RegisterTriggers: %v", err)
	}

	// Sample points are at feet-Y = fy-0.3, fy, fy+0.3 (fy = py + 1.0).
	// Blocked iff any sample lands in [5.0, 5.2].
	cases := []struct {
		name      string
		px, py    float32
		wantBlock bool
	}{
		{"feet 4.6 (samples 4.3/4.6/4.9 — all below wall)", 5.0, 3.6, false},
		{"feet 5.0 (samples 4.7/5.0/5.3 — 5.0 hits)", 5.0, 4.0, true},
		{"feet 5.1 (samples 4.8/5.1/5.4 — 5.1 hits)", 5.0, 4.1, true},
		{"feet 5.2 (samples 4.9/5.2/5.5 — 5.2 hits)", 5.0, 4.2, true},
		{"feet 5.3 (samples 5.0/5.3/5.6 — 5.0 hits)", 5.0, 4.3, true},
		{"feet 5.6 (samples 5.3/5.6/5.9 — all above wall)", 5.0, 4.6, false},
	}
	for _, c := range cases {
		got := s.isPositionBlocked(c.px, c.py)
		if got != c.wantBlock {
			t.Errorf("%s: isPositionBlocked(%v, %v) = %v, want %v (feet Y=%v, wall [5.0,5.2])",
				c.name, c.px, c.py, got, c.wantBlock, c.py+1.0)
		}
	}
}

// TestIsPositionBlocked_ThinWallTunneling demonstrates the limitation: a
// wall thinner than the 0.3 sample spacing can be tunneled through when the
// player's per-tick movement (0.4 tiles) lands the feet between sample hits.
// This is a known limitation of point-sampling collision, not a bug in
// Contains. It is documented here to prevent future regressions and to
// warn map authors against walls thinner than ~0.3 tiles.
func TestIsPositionBlocked_ThinWallTunneling(t *testing.T) {
	// Wall 0.1 tiles thick at feet-Y [5.05, 5.15] — thinner than 0.3 spacing.
	zones := []*Zone{{ID: "razor", Shape: ShapeRect, X: 0, Y: 5.05, W: 20, H: 0.1}}
	s := &Simulator{
		zoneReg: NewZoneRegistry(zones, 20, 20),
		extMgr:  NewExtensionManager(slog.Default()),
	}
	if err := s.extMgr.Register([]byte(`{"extension_id":"ext-walls","heartbeat_interval_s":10}`)); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := s.extMgr.RegisterTriggers([]byte(`{
		"extension_id": "ext-walls",
		"gate_triggers": [{"zone_id": "razor", "behavior": "block"}]
	}`)); err != nil {
		t.Fatalf("RegisterTriggers: %v", err)
	}

	// feet at 5.25: samples at 4.95, 5.25, 5.55 — none in [5.05, 5.15].
	// The player's feet box straddles the wall but no sample point lands
	// inside it, so the wall is NOT detected. This is the tunneling case.
	got := s.isPositionBlocked(5.0, 4.25) // py=4.25 -> feet=5.25
	if got {
		t.Errorf("expected tunneling (0.1-tile wall missed at feet=5.25), but got blocked. "+
			"Sampling may have changed — update this test and the doc note in 21-map-design-guide.md")
	}
}
