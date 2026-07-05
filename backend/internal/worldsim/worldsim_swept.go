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

// segmentIntersectsPolygonExpanded tests the segment against a polygon
// expanded by radius r (Minkowski sum of polygon + disc of radius r). This
// is done by testing the segment against the original polygon (catches
// crossings and containment) plus testing the segment against each edge
// expanded as a capsule (segment + disc), approximated by testing the
// segment against a circle of radius r at each edge endpoint, plus the
// edge itself. For a true capsule test we'd need segment-to-segment
// distance, but the endpoint-circle + edge-intersection approximation is
// safe (over-approximates slightly near vertices).
func segmentIntersectsPolygonExpanded(x0, y0, x1, y1 float32, poly [][2]float32, r float32) bool {
	// Original polygon test: crossings + containment.
	if segmentIntersectsPolygon(x0, y0, x1, y1, poly) {
		return true
	}
	// Expanded test: for each edge, check if the movement segment comes
	// within r of the edge. Approximate by checking the segment against a
	// circle of radius r at each polygon vertex (covers the rounded caps of
	// the Minkowski sum) and against the edge itself (already done above).
	// The edge-interior gap (parallel segments within r) is not covered by
	// vertex circles alone, so also test the segment against the edge's
	// nearest point via a segment-segment distance check.
	n := len(poly)
	for i := 0; i < n; i++ {
		ax, ay := poly[i][0], poly[i][1]
		// Vertex cap: circle of radius r at each vertex.
		if segmentIntersectsCircle(x0, y0, x1, y1, ax, ay, r) {
			return true
		}
		// Edge interior: nearest point on movement segment to this edge.
		bx, by := poly[(i+1)%n][0], poly[(i+1)%n][1]
		if segmentSegmentDistLE(x0, y0, x1, y1, ax, ay, bx, by, r) {
			return true
		}
	}
	return false
}

// segmentSegmentDistLE reports whether the shortest distance between segment
// P0-P1 and segment Q0-Q1 is <= r. Used for the polygon expansion (testing
// if the movement segment comes within the player radius of a polygon edge).
func segmentSegmentDistLE(p0x, p0y, p1x, p1y, q0x, q0y, q1x, q1y, r float32) bool {
	// If the segments intersect, distance is 0.
	if segmentsIntersect(p0x, p0y, p1x, p1y, q0x, q0y, q1x, q1y) {
		return true
	}
	// Otherwise, the minimum distance is the min of the distances from each
	// endpoint to the other segment. (For non-intersecting segments in 2D,
	// the closest pair involves at least one endpoint.)
	r2 := r * r
	if pointSegmentDistSq(p0x, p0y, q0x, q0y, q1x, q1y) <= r2 {
		return true
	}
	if pointSegmentDistSq(p1x, p1y, q0x, q0y, q1x, q1y) <= r2 {
		return true
	}
	if pointSegmentDistSq(q0x, q0y, p0x, p0y, p1x, p1y) <= r2 {
		return true
	}
	if pointSegmentDistSq(q1x, q1y, p0x, p0y, p1x, p1y) <= r2 {
		return true
	}
	return false
}

// pointSegmentDistSq returns the squared distance from point P to segment A-B.
func pointSegmentDistSq(px, py, ax, ay, bx, by float32) float32 {
	dx := bx - ax
	dy := by - ay
	lensq := dx*dx + dy*dy
	t := float32(0)
	if lensq > 0 {
		t = ((px-ax)*dx + (py-ay)*dy) / lensq
	}
	if t < 0 {
		t = 0
	} else if t > 1 {
		t = 1
	}
	cx := ax + t*dx
	cy := ay + t*dy
	ddx := px - cx
	ddy := py - cy
	return ddx*ddx + ddy*ddy
}
