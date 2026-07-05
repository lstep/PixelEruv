package worldsim

import (
	"fmt"
	"math"
)

// ZoneShape is the geometric shape of a zone.
type ZoneShape int

const (
	ShapeRect ZoneShape = iota
	ShapeCircle
	ShapePolygon
)

// Zone is a region on the map with spatial and metadata properties.
type Zone struct {
	ID         string
	Shape      ZoneShape
	X, Y       float32 // top-left in tile coords
	W, H       float32 // width/height in tile coords (rect/circle bounding box)
	Radius     float32 // circle radius in tile coords
	Polygon    [][2]float32 // polygon vertices in tile coords (relative to X,Y)
	ZoneType   string  // "meeting", "water", "work", "silent", etc.
	IsExclusive bool
	Mobility   string  // "static" or "mobile"
}

// Contains returns true if the point (px, py) in tile coords is inside the zone.
func (z *Zone) Contains(px, py float32) bool {
	switch z.Shape {
	case ShapeRect:
		return px >= z.X && px <= z.X+z.W &&
			py >= z.Y && py <= z.Y+z.H
	case ShapeCircle:
		cx, cy := z.X+z.Radius, z.Y+z.Radius
		dx, dy := px-cx, py-cy
		return dx*dx+dy*dy <= z.Radius*z.Radius
	case ShapePolygon:
		// Polygon vertices are stored relative to (z.X, z.Y) in tile coords.
		// Translate to absolute coords for the point-in-polygon test.
		abs := make([][2]float32, len(z.Polygon))
		for i, v := range z.Polygon {
			abs[i] = [2]float32{v[0] + z.X, v[1] + z.Y}
		}
		return pointInPolygon(px, py, abs)
	}
	return false
}

// pointInPolygon uses the ray-casting algorithm.
func pointInPolygon(px, py float32, poly [][2]float32) bool {
	inside := false
	n := len(poly)
	j := n - 1
	for i := 0; i < n; i++ {
		xi, yi := poly[i][0], poly[i][1]
		xj, yj := poly[j][0], poly[j][1]
		if ((yi > py) != (yj > py)) &&
			(px < (xj-xi)*(py-yi)/(yj-yi)+xi) {
			inside = !inside
		}
		j = i
	}
	return inside
}

// ZoneRegistry holds all zones for a map, with a pre-rasterized tile set
// for O(1) point-in-zone lookup of static zones.
type ZoneRegistry struct {
	zones   []*Zone
	tileSet map[string]map[string]bool // [tileKey]zoneID set
}

// NewZoneRegistry builds a registry from zones and pre-rasterizes static zones.
func NewZoneRegistry(zones []*Zone, mapW, mapH int) *ZoneRegistry {
	r := &ZoneRegistry{
		zones:   zones,
		tileSet: make(map[string]map[string]bool),
	}
	for _, z := range zones {
		if z.Mobility == "mobile" {
			continue // mobile zones are evaluated per-tick, not rasterized
		}
		// Rasterize: check every tile in the zone's bounding box.
		// For polygons, Tiled stores width=0/height=0, so compute the
		// bounding box from the vertices instead.
		bxMin, byMin, bxMax, byMax := z.X, z.Y, z.X+z.W, z.Y+z.H
		if z.Shape == ShapePolygon && len(z.Polygon) > 0 {
			bxMin, byMin = z.Polygon[0][0]+z.X, z.Polygon[0][1]+z.Y
			bxMax, byMax = bxMin, byMin
			for _, v := range z.Polygon {
				ax, ay := v[0]+z.X, v[1]+z.Y
				if ax < bxMin {
					bxMin = ax
				}
				if ay < byMin {
					byMin = ay
				}
				if ax > bxMax {
					bxMax = ax
				}
				if ay > byMax {
					byMax = ay
				}
			}
		}
		minX := int(math.Floor(float64(bxMin)))
		minY := int(math.Floor(float64(byMin)))
		maxX := int(math.Ceil(float64(bxMax)))
		maxY := int(math.Ceil(float64(byMax)))
		for ty := minY; ty < maxY && ty < mapH; ty++ {
			for tx := minX; tx < maxX && tx < mapW; tx++ {
				if tx < 0 || ty < 0 {
					continue
				}
				// Check tile center.
				cx, cy := float32(tx)+0.5, float32(ty)+0.5
				if z.Contains(cx, cy) {
					key := tileKey(tx, ty)
					if r.tileSet[key] == nil {
						r.tileSet[key] = make(map[string]bool)
					}
					r.tileSet[key][z.ID] = true
				}
			}
		}
	}
	return r
}

// ZonesAt returns the IDs of zones that contain the tile at (tx, ty).
func (r *ZoneRegistry) ZonesAt(tx, ty int) []string {
	key := tileKey(tx, ty)
	set := r.tileSet[key]
	if len(set) == 0 {
		return nil
	}
	result := make([]string, 0, len(set))
	for id := range set {
		result = append(result, id)
	}
	return result
}

// ZonesAtPoint returns zones containing the point (px, py) in tile coords.
// Checks both rasterized static zones and mobile zones.
func (r *ZoneRegistry) ZonesAtPoint(px, py float32) []string {
	var result []string
	// Check rasterized static zones via tile lookup.
	tx, ty := int(math.Floor(float64(px))), int(math.Floor(float64(py)))
	for id := range r.tileSet[tileKey(tx, ty)] {
		result = append(result, id)
	}
	// Check mobile zones with distance.
	for _, z := range r.zones {
		if z.Mobility != "mobile" {
			continue
		}
		if z.Contains(px, py) {
			result = append(result, z.ID)
		}
	}
	return result
}

// AllZones returns all registered zones.
func (r *ZoneRegistry) AllZones() []*Zone {
	return r.zones
}

func tileKey(x, y int) string {
	return fmt.Sprintf("%d:%d", x, y)
}
