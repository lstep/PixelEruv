package worldsim

import (
	"context"
)

// MovementSystem moves all player avatars one tick's worth of input,
// resolving collisions with swept (segment-vs-shape) zone checks so walls
// thinner than the per-tick movement distance cannot be tunneled through.
// After movement, it updates stationary ticks (for proximity A/V gating)
// and repositions each player's mobile proximity zone to follow the avatar.
//
// Field ownership (writes):
//   - Entity.Position (X, Y, Dir)
//   - Entity.dirtyPosition
//   - Entity.stationaryTicks
//   - Entity.mobileZone.X, Entity.mobileZone.Y
type MovementSystem struct {
	extMgr *ExtensionManager
}

// NewMovementSystem constructs a MovementSystem. extMgr is needed for
// IsZoneBlocked gate-trigger checks during collision resolution.
func NewMovementSystem(extMgr *ExtensionManager) *MovementSystem {
	return &MovementSystem{extMgr: extMgr}
}

// Step runs the movement system: applies input-driven movement with collision,
// updates stationary ticks, and repositions mobile proximity zones.
// Caller must hold s.mu (the world mutex).
func (m *MovementSystem) Step(_ context.Context, w *World) {
	m.runMovement(w)
	m.updateStationaryTicks(w)
	m.updateMobileZones(w)
}

func (m *MovementSystem) runMovement(w *World) {
	speed := float32(0.4) // tiles per tick (~8 tiles/sec at 20Hz)
	for _, e := range w.entities {
		if e.NetworkSession == nil || e.Position == nil {
			continue
		}
		input := e.NetworkSession.Input
		if input == nil {
			continue
		}

		dx, dy := float32(0), float32(0)
		if input.Up {
			dy -= 1
		}
		if input.Down {
			dy += 1
		}
		if input.Left {
			dx -= 1
		}
		if input.Right {
			dx += 1
		}

		// Normalize diagonal
		if dx != 0 && dy != 0 {
			dx *= float32(0.7071)
			dy *= float32(0.7071)
		}

		if dx == 0 && dy == 0 {
			continue
		}

		newX := e.Position.X + dx*speed
		newY := e.Position.Y + dy*speed

		md := w.maps[e.Position.MapId]
		zr := w.zones[e.Position.MapId]
		if md != nil {
			// Clamp to map bounds.
			newX = clamp(newX, 0, float32(md.Width-1))
			newY = clamp(newY, 0, float32(md.Height-1))

			// Collision check: try X and Y independently so the avatar
			// slides along walls instead of sticking. Swept (segment-vs-
			// shape) checks catch walls thinner than the per-tick movement
			// that point-sampling at the destination would miss.
			if m.isMoveBlocked(zr, md, e.Position.X, e.Position.Y, newX, e.Position.Y) {
				newX = e.Position.X
			}
			if m.isMoveBlocked(zr, md, newX, e.Position.Y, newX, newY) {
				newY = e.Position.Y
			}
			// Diagonal guard: if both axes moved, check the full diagonal
			// segment. The X-then-Y decomposition can skip a wall that the
			// diagonal crosses but neither axis-aligned segment does (the
			// X move jumps past a thin wall, then the Y move sits outside
			// its X range). If the diagonal is blocked, revert Y to slide
			// along the X axis.
			if newX != e.Position.X && newY != e.Position.Y {
				if m.isMoveBlocked(zr, md, e.Position.X, e.Position.Y, newX, newY) {
					newY = e.Position.Y
				}
			}
		} else {
			// Fallback: no map data, use hardcoded bounds.
			newX = clamp(newX, 1, 18)
			newY = clamp(newY, 1, 18)
		}

		if newX != e.Position.X || newY != e.Position.Y {
			e.Position.X = newX
			e.Position.Y = newY
			e.dirtyPosition = true

			// Update direction
			if absF(dx) > absF(dy) {
				if dx > 0 {
					e.Position.Dir = 2 // right
				} else {
					e.Position.Dir = 1 // left
				}
			} else {
				if dy > 0 {
					e.Position.Dir = 0 // down
				} else {
					e.Position.Dir = 3 // up
				}
			}
		} else if dx != 0 || dy != 0 {
			// Movement was attempted but fully blocked (e.g. walking
			// directly into a wall). Mark dirty so the client gets a
			// position correction even though the position didn't change —
			// otherwise the client's prediction runs ahead through the
			// wall and never gets snapped back.
			e.dirtyPosition = true
		}
	}
}

// updateStationaryTicks counts consecutive ticks without movement per player.
// dirtyPosition is set by runMovement when the player moved this tick, and
// cleared at the end of the tick (by ReplicationSystem). This update runs
// before that clear, so it sees the current tick's movement state.
func (m *MovementSystem) updateStationaryTicks(w *World) {
	for _, e := range w.entities {
		if e.NetworkSession == nil {
			continue
		}
		if e.dirtyPosition {
			e.stationaryTicks = 0
		} else {
			e.stationaryTicks++
		}
	}
}

// updateMobileZones moves each player's proximity circle to follow their
// avatar's current position (after movement was applied this tick). Centered
// at the feet to match where zone detection evaluates membership. Must happen
// before zone detection so the zone check sees up-to-date positions.
func (m *MovementSystem) updateMobileZones(w *World) {
	for _, e := range w.entities {
		if e.mobileZone != nil && e.Position != nil {
			e.mobileZone.X = e.Position.X - proximityRadius
			e.mobileZone.Y = e.Position.Y + avatarFeetYOffset - proximityRadius
		}
	}
}

// isMoveBlocked checks whether the movement segment from (oldX, oldY) to
// (newX, newY) in tile coords is blocked. Zone collision uses swept
// (segment-vs-shape) tests in continuous space, evaluated at the avatar's
// feet (Position.Y + avatarFeetYOffset), so walls thinner than the per-tick
// movement distance cannot be tunneled through. The Walls tile-layer fallback
// is checked at both endpoints' feet tiles (the tile grid is integer-indexed
// and movement is < 1 tile/tick, so endpoint sampling suffices there).
func (m *MovementSystem) isMoveBlocked(zr *ZoneRegistry, md *MapData, oldX, oldY, newX, newY float32) bool {
	// Translate to feet space.
	ofy := oldY + avatarFeetYOffset
	nfy := newY + avatarFeetYOffset
	r := playerCollisionRadius

	// Zone gate triggers: swept segment-vs-shape against each blocked zone.
	// Each shape is expanded by the player collision radius (Minkowski sum)
	// so the feet center stops `r` tiles before the wall edge, matching the
	// old 5-point sampling box width.
	if zr != nil {
		for _, z := range zr.zones {
			if !m.extMgr.IsZoneBlocked(z.ID) {
				continue
			}
			switch z.Shape {
			case ShapeRect:
				if segmentIntersectsRect(oldX, ofy, newX, nfy,
					z.X-r, z.Y-r, z.W+2*r, z.H+2*r) {
					return true
				}
			case ShapeCircle:
				cx, cy := z.X+z.Radius, z.Y+z.Radius
				if segmentIntersectsCircle(oldX, ofy, newX, nfy, cx, cy, z.Radius+r) {
					return true
				}
			case ShapePolygon:
				// Expand the polygon's bounding box by the radius. This is an
				// over-approximation (the expanded box is larger than the
				// true Minkowski sum of polygon + circle), so it may stop the
				// player slightly early near concave corners — safe but not
				// precise. A true polygon+circle Minkowski sum would require
				// offsetting each edge along its outward normal.
				abs := make([][2]float32, len(z.Polygon))
				for i, v := range z.Polygon {
					abs[i] = [2]float32{v[0] + z.X, v[1] + z.Y}
				}
				if segmentIntersectsPolygonExpanded(oldX, ofy, newX, nfy, abs, r) {
					return true
				}
			}
		}
	}
	// Fallback: Walls tile layer collision (tile-based by nature), at both
	// endpoints' feet tiles. Movement is < 1 tile/tick so if either endpoint
	// is in a blocked tile, the segment crossed it.
	//
	// Coordinate convention: the sprite is 1 tile wide centered on
	// Position.X, and feet sit at Position.Y + avatarFeetYOffset (origin
	// 0.5/0.75 on a 64px frame placed at (pos.X*32, pos.Y*32+16)). So
	// Position.X = N is the LEFT edge of tile N and feet at M is the TOP
	// edge of tile M — tile index = floor(Position coord). The sprite's
	// leading edge depends on movement direction: right edge (Position.X
	// +0.5) when moving +X, left edge (Position.X -0.5) when moving -X,
	// center when X is static. The feet are a single point, so floor(feet)
	// is direction-independent. Checking the +edge unconditionally (the old
	// int(x+0.5) bias) only matched the leading edge for +X/+Y movement;
	// -X/-Y movement checked the trailing edge and tunneled ~1 tile deep.
	if md != nil {
		const half = float32(0.5)
		var ledOldX, ledNewX float32
		switch {
		case newX > oldX:
			ledOldX, ledNewX = oldX+half, newX+half
		case newX < oldX:
			ledOldX, ledNewX = oldX-half, newX-half
		default:
			ledOldX, ledNewX = oldX, newX
		}
		if md.IsBlocked(int(ledOldX), int(ofy)) {
			return true
		}
		if md.IsBlocked(int(ledNewX), int(nfy)) {
			return true
		}
	}
	return false
}
