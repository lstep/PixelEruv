package worldsim

// ZoneShape is the geometric shape of a zone.
type ZoneShape int

const (
	ShapeRect ZoneShape = iota
	ShapeCircle
	ShapePolygon
)

// Zone is a region on the map with spatial and metadata properties.
// All coordinates are in tile units (continuous, not grid-aligned) —
// e.g. X=5.5 means 5.5 tiles, not tile index 5. Positions are float and
// movement is sub-tile (0.4 tiles/tick); "tile units" is the scale, not
// the granularity.
type Zone struct {
	ID         string
	Shape      ZoneShape
	X, Y       float32       // top-left in tile coords
	W, H       float32       // width/height in tile coords (rect/circle bounding box)
	Radius     float32       // circle radius in tile coords
	Polygon    [][2]float32  // polygon vertices in tile coords (relative to X,Y)
	ZoneType   string        // "meeting", "water", "work", "silent", etc.
	IsExclusive bool
	Mobility   string        // "static" or "mobile"
	AvEnabled  bool          // zone has av_enabled Tiled property → zone-based A/V room
}

// Contains returns true if the point (px, py) in tile coords is inside the zone.
// All checks are done in continuous space — no tile grid approximation.
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

// ZoneRegistry holds all zones for a map. Zone membership is checked
// in continuous space directly against zone shapes — no tile rasterization.
type ZoneRegistry struct {
	zones []*Zone
}

// NewZoneRegistry builds a registry from zones.
func NewZoneRegistry(zones []*Zone, mapW, mapH int) *ZoneRegistry {
	return &ZoneRegistry{zones: zones}
}

// AddZone adds a zone to the registry. Used for mobile zones (proximity
// circles that follow player avatars). Caller must hold the Simulator lock.
func (r *ZoneRegistry) AddZone(z *Zone) {
	r.zones = append(r.zones, z)
}

// RemoveZone removes a zone by ID. Used when a player despawns and their
// mobile proximity zone is no longer needed. Caller must hold the Simulator lock.
func (r *ZoneRegistry) RemoveZone(zoneID string) {
	for i, z := range r.zones {
		if z.ID == zoneID {
			r.zones = append(r.zones[:i], r.zones[i+1:]...)
			return
		}
	}
}

// ZonesAtPoint returns the IDs of zones that contain the point (px, py)
// in tile coords. Checks all zones directly in continuous space.
func (r *ZoneRegistry) ZonesAtPoint(px, py float32) []string {
	var result []string
	for _, z := range r.zones {
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
