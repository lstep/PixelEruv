package worldsim

import (
	pb "github.com/lstep/pixeleruv/backend/internal/pb"
)

// AOI (Area of Interest) grid constants.
//
// Cell size is in tile units. Subscribe radius is the distance (in cells) at
// which an entity enters a client's view. Unsubscribe radius is the distance
// at which an entity leaves. The hysteresis band (subscribe < unsubscribe)
// prevents spawn/despawn storms when an entity oscillates across the boundary.
//
// With cellSize=16 and subscribeRadius=3, a client sees entities within
// 7x7 = 49 cells = 112x112 tiles. For a 50x50 map this covers everything
// (degrades to current whole-map behavior). For a 2000x2000 map it covers
// 0.3% of the world — the actual scaling win.
const (
	aoiCellSize          = 16 // tiles per cell
	aoiSubscribeRadius   = 3  // cells — entity enters client view
	aoiUnsubscribeRadius = 4  // cells — entity leaves client view
)

// AOIGrid is a spatial hash grid that indexes entities by tile position for
// efficient area-of-interest queries. One grid per map. Rebuilt from scratch
// each tick before replication — no incremental maintenance needed across
// provision/despawn/portal-transition.
type AOIGrid struct {
	cellSize int
	cells    map[[2]int]map[string]*Entity // cell coords -> entity_id -> entity
}

// NewAOIGrid creates an empty grid with the given cell size in tiles.
func NewAOIGrid(cellSize int) *AOIGrid {
	return &AOIGrid{cellSize: cellSize, cells: make(map[[2]int]map[string]*Entity)}
}

// CellOf returns the grid cell coordinates for a tile position.
func (g *AOIGrid) CellOf(x, y float32) [2]int {
	return [2]int{int(x) / g.cellSize, int(y) / g.cellSize}
}

// Insert adds an entity to the grid at its current position. No-op if the
// entity has no position.
func (g *AOIGrid) Insert(e *Entity) {
	if e.Position == nil {
		return
	}
	c := g.CellOf(e.Position.X, e.Position.Y)
	cell, ok := g.cells[c]
	if !ok {
		cell = make(map[string]*Entity)
		g.cells[c] = cell
	}
	cell[e.ID] = e
}

// EntitiesInRadius returns all entities within radiusCells of the cell
// containing the given position (Chebyshev/square distance). The returned
// map is keyed by entity ID. Returns nil if pos is nil.
func (g *AOIGrid) EntitiesInRadius(pos *pb.Position, radiusCells int) map[string]*Entity {
	if pos == nil {
		return nil
	}
	result := make(map[string]*Entity)
	cx, cy := int(pos.X)/g.cellSize, int(pos.Y)/g.cellSize
	for dx := -radiusCells; dx <= radiusCells; dx++ {
		for dy := -radiusCells; dy <= radiusCells; dy++ {
			if cell, ok := g.cells[[2]int{cx + dx, cy + dy}]; ok {
				for id, e := range cell {
					result[id] = e
				}
			}
		}
	}
	return result
}
