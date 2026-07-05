package worldsim

// Swept (segment-vs-shape) collision tests. Used by isMoveBlocked to detect
// walls that fall between the per-tick start and end positions, which
// point-sampling at the destination can miss (tunneling). All helpers operate
// in continuous tile coords already translated to feet space by the caller.

// segmentIntersectsRect reports whether the segment from (x0,y0) to (x1,y1)
// intersects the axis-aligned rect [rx, rx+rw] x [ry, ry+rh]. Uses the slab
// method: parametrize the segment as P(t) = P0 + t*(P1-P0), t in [0,1], and
// find the overlap of the t-ranges where the segment is inside each axis slab.
func segmentIntersectsRect(x0, y0, x1, y1, rx, ry, rw, rh float32) bool {
	rx1 := rx + rw
	ry1 := ry + rh
	dx := x1 - x0
	dy := y1 - y0

	// Start with the full segment parameter range.
	t0 := float32(0)
	t1 := float32(1)

	// X slab.
	if dx == 0 {
		// Segment is parallel to the Y axis. It can only be inside the X
		// slab if x0 is within [rx, rx1]; otherwise no intersection.
		if x0 < rx || x0 > rx1 {
			return false
		}
	} else {
		ta := (rx - x0) / dx
		tb := (rx1 - x0) / dx
		if ta > tb {
			ta, tb = tb, ta
		}
		if ta > t0 {
			t0 = ta
		}
		if tb < t1 {
			t1 = tb
		}
		if t0 > t1 {
			return false
		}
	}

	// Y slab.
	if dy == 0 {
		if y0 < ry || y0 > ry1 {
			return false
		}
	} else {
		ta := (ry - y0) / dy
		tb := (ry1 - y0) / dy
		if ta > tb {
			ta, tb = tb, ta
		}
		if ta > t0 {
			t0 = ta
		}
		if tb < t1 {
			t1 = tb
		}
		if t0 > t1 {
			return false
		}
	}

	// Hit iff the overlapping t-range intersects [0, 1]. We already clipped
	// t0/t1 to [0,1] above, so any remaining overlap is a hit.
	return t0 <= t1
}

// segmentIntersectsCircle reports whether the segment from (x0,y0) to
// (x1,y1) comes within r of (cx, cy). Uses the standard closest-point-on-
// segment distance test.
func segmentIntersectsCircle(x0, y0, x1, y1, cx, cy, r float32) bool {
	dx := x1 - x0
	dy := y1 - y0
	lensq := dx*dx + dy*dy

	// Project (C - P0) onto (P1 - P0) to find the closest parameter t.
	// Clamp t to [0, 1] so the closest point is on the segment.
	t := float32(0)
	if lensq > 0 {
		t = ((cx-x0)*dx + (cy-y0)*dy) / lensq
	}
	if t < 0 {
		t = 0
	} else if t > 1 {
		t = 1
	}

	px := x0 + t*dx
	py := y0 + t*dy
	ddx := px - cx
	ddy := py - cy
	return ddx*ddx+ddy*ddy <= r*r
}

// segmentIntersectsPolygon reports whether the segment from (x0,y0) to
// (x1,y1) intersects the polygon (absolute vertex coords). True if any edge
// of the polygon is crossed by the segment, or if either endpoint is inside
// the polygon (segment fully contained).
func segmentIntersectsPolygon(x0, y0, x1, y1 float32, poly [][2]float32) bool {
	// Endpoint containment covers the case where the segment is entirely
	// inside the polygon (no edge crossing).
	if pointInPolygon(x0, y0, poly) || pointInPolygon(x1, y1, poly) {
		return true
	}
	n := len(poly)
	for i := 0; i < n; i++ {
		ax, ay := poly[i][0], poly[i][1]
		bx, by := poly[(i+1)%n][0], poly[(i+1)%n][1]
		if segmentsIntersect(x0, y0, x1, y1, ax, ay, bx, by) {
			return true
		}
	}
	return false
}

// segmentsIntersect reports whether segment A-B and segment C-D intersect.
// Uses the standard orientation-test approach.
func segmentsIntersect(ax, ay, bx, by, cx, cy, dx, dy float32) bool {
	d1 := cross(cx, cy, dx, dy, ax, ay)
	d2 := cross(cx, cy, dx, dy, bx, by)
	d3 := cross(ax, ay, bx, by, cx, cy)
	d4 := cross(ax, ay, bx, by, dx, dy)
	if ((d1 > 0) != (d2 > 0)) && ((d3 > 0) != (d4 > 0)) {
		return true
	}
	// Collinear / touching cases: treat as non-intersecting for collision
	// purposes (a wall grazed exactly on its edge does not block). This
	// avoids false positives when a segment runs along a polygon edge.
	return false
}

// cross returns the 2D cross product of (C->D) x (C->P), i.e. the signed
// area of the triangle CDP. Sign tells which side of C->D the point P is on.
func cross(cx, cy, dx, dy, px, py float32) float32 {
	return (dx-cx)*(py-cy) - (dy-cy)*(px-cx)
}
