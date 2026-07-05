package worldsim

import "testing"

func TestSegmentIntersectsRect(t *testing.T) {
	// Rect covers [5,6] x [5,6].
	const rx, ry, rw, rh = 5, 5, 1, 1
	cases := []struct {
		name                       string
		x0, y0, x1, y1             float32
		want                       bool
	}{
		{"crosses left-to-right through rect", 4, 5.5, 7, 5.5, true},
		{"crosses top-to-bottom through rect", 5.5, 4, 5.5, 7, true},
		{"diagonal through rect", 4, 4, 7, 7, true},
		{"starts inside rect", 5.5, 5.5, 7, 7, true},
		{"ends inside rect", 4, 4, 5.5, 5.5, true},
		{"fully left of rect", 3, 5.5, 4, 5.5, false},
		{"fully right of rect", 7, 5.5, 8, 5.5, false},
		{"fully above rect", 5.5, 3, 5.5, 4, false},
		{"fully below rect", 5.5, 7, 5.5, 8, false},
		{"parallel to X, inside slab, no Y overlap", 4, 7, 7, 7, false},
		{"parallel to Y, inside slab, no X overlap", 7, 4, 7, 7, false},
		{"grazes top edge (collinear)", 4, 5, 7, 5, true},
		{"segment entirely inside rect", 5.2, 5.2, 5.8, 5.8, true},
	}
	for _, c := range cases {
		got := segmentIntersectsRect(c.x0, c.y0, c.x1, c.y1, rx, ry, rw, rh)
		if got != c.want {
			t.Errorf("%s: segmentIntersectsRect(%v,%v,%v,%v, %v,%v,%v,%v) = %v, want %v",
				c.name, c.x0, c.y0, c.x1, c.y1, rx, ry, rw, rh, got, c.want)
		}
	}
}

func TestSegmentIntersectsCircle(t *testing.T) {
	// Circle center (5,5) radius 1.
	const cx, cy, r = 5, 5, 1
	cases := []struct {
		name                   string
		x0, y0, x1, y1         float32
		want                   bool
	}{
		{"passes through center", 3, 5, 7, 5, true},
		{"tangent from outside", 3, 6, 7, 6, true}, // closest point at (5,6), dist=1
		{"misses above", 3, 7, 7, 7, false},
		{"starts inside circle", 5, 5, 8, 5, true},
		{"ends inside circle", 3, 5, 5, 5, true},
		{"segment endpoint closest, inside", 0, 0, 5.5, 5.5, true},
		{"segment endpoint closest, outside", 0, 0, 4, 4, false}, // dist(4,4)->(5,5)=1.414>1
		{"vertical through", 5, 3, 5, 7, true},
	}
	for _, c := range cases {
		got := segmentIntersectsCircle(c.x0, c.y0, c.x1, c.y1, cx, cy, r)
		if got != c.want {
			t.Errorf("%s: segmentIntersectsCircle(%v,%v,%v,%v, %v,%v,%v) = %v, want %v",
				c.name, c.x0, c.y0, c.x1, c.y1, cx, cy, r, got, c.want)
		}
	}
}

func TestSegmentIntersectsPolygon(t *testing.T) {
	// Diamond around (5,5): vertices (5,4)(6,5)(5,6)(4,5).
	poly := [][2]float32{{5, 4}, {6, 5}, {5, 6}, {4, 5}}
	cases := []struct {
		name           string
		x0, y0, x1, y1 float32
		want           bool
	}{
		{"crosses horizontally through", 3, 5, 7, 5, true},
		{"crosses vertically through", 5, 3, 5, 7, true},
		{"starts inside", 5, 5, 8, 5, true},
		{"ends inside", 3, 5, 5, 5, true},
		{"fully outside (left)", 2, 5, 3, 5, false},
		{"fully outside (above)", 5, 2, 5, 3, false},
		{"segment fully inside polygon", 4.8, 5, 5.2, 5, true},
		{"endpoint touches a vertex (counts as hit)", 3, 3, 5, 4, true},
		{"segment passes near but misses", 3, 3, 4.2, 4.2, false},
	}
	for _, c := range cases {
		got := segmentIntersectsPolygon(c.x0, c.y0, c.x1, c.y1, poly)
		if got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
