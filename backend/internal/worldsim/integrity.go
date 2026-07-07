package worldsim

import (
	"fmt"
	"log/slog"
	"strings"
)

// CheckLevel is the severity of an integrity issue.
type CheckLevel int

const (
	LevelError   CheckLevel = iota // blocks normal operation
	LevelWarning                   // suspicious but not fatal
	LevelInfo                      // informational
)

func (l CheckLevel) String() string {
	switch l {
	case LevelError:
		return "ERROR"
	case LevelWarning:
		return "WARN"
	case LevelInfo:
		return "INFO"
	default:
		return "UNKNOWN"
	}
}

// CheckResult is a single integrity issue found by CheckMapIntegrity.
type CheckResult struct {
	Level   CheckLevel
	Layer   string // layer name, or "" for map-level
	Zone    string // zone ID, or "" for non-zone issues
	Message string
}

// String formats a check result for logging.
func (r CheckResult) String() string {
	parts := []string{r.Level.String()}
	if r.Layer != "" {
		parts = append(parts, "layer="+r.Layer)
	}
	if r.Zone != "" {
		parts = append(parts, "zone="+r.Zone)
	}
	parts = append(parts, r.Message)
	return strings.Join(parts, " ")
}

// knownZoneTypes is the set of zone_type values that have built-in handling.
// Unknown values are a warning (the kernel doesn't interpret them, but a
// typo could mean the extension won't recognize the zone).
var knownZoneTypes = map[string]bool{
	"wall":    true,
	"meeting": true,
	"water":   true,
	"work":    true,
	"silent":  true,
	"spawn":   true,
}

// CheckMapIntegrity validates a parsed Tiled map for common issues:
// missing required layers, unnamed zones, duplicate IDs, shape constraints,
// tile size mismatches, etc. Returns a list of issues found.
func CheckMapIntegrity(md *MapData) []CheckResult {
	var results []CheckResult

	if md == nil {
		return []CheckResult{{Level: LevelError, Message: "map data is nil"}}
	}

	// --- Map-level checks ---
	if md.Width <= 0 || md.Height <= 0 {
		results = append(results, CheckResult{
			Level:   LevelError,
			Message: fmt.Sprintf("invalid map dimensions: %dx%d", md.Width, md.Height),
		})
	}

	// --- Collision grid checks ---
	if md.Collision == nil {
		results = append(results, CheckResult{
			Level:   LevelWarning,
			Message: "collision grid is nil — no Walls layer found",
		})
	} else if len(md.Collision) != md.Height {
		results = append(results, CheckResult{
			Level:   LevelError,
			Layer:   "Walls",
			Message: fmt.Sprintf("collision grid height %d != map height %d", len(md.Collision), md.Height),
		})
	} else {
		for y, row := range md.Collision {
			if len(row) != md.Width {
				results = append(results, CheckResult{
					Level:   LevelError,
					Layer:   "Walls",
					Message: fmt.Sprintf("collision row %d width %d != map width %d", y, len(row), md.Width),
				})
				break
			}
		}
	}

	// --- Zone checks ---
	zoneIDs := make(map[string]bool)
	for _, z := range md.Zones {
		// Duplicate zone ID.
		if zoneIDs[z.ID] {
			results = append(results, CheckResult{
				Level:   LevelError,
				Zone:    z.ID,
				Message: "duplicate zone ID",
			})
		}
		zoneIDs[z.ID] = true

		// Zero-size zone.
		if z.Shape == ShapeRect || z.Shape == ShapeCircle {
			if z.W <= 0 || z.H <= 0 {
				results = append(results, CheckResult{
					Level:   LevelError,
					Zone:    z.ID,
					Message: fmt.Sprintf("zero or negative size: w=%.2f h=%.2f", z.W, z.H),
				})
			}
		}

		// Circle zone: radius must be positive.
		if z.Shape == ShapeCircle && z.Radius <= 0 {
			results = append(results, CheckResult{
				Level:   LevelError,
				Zone:    z.ID,
				Message: fmt.Sprintf("circle zone has invalid radius: %.2f", z.Radius),
			})
		}

		// Polygon zone: needs at least 3 vertices.
		if z.Shape == ShapePolygon && len(z.Polygon) < 3 {
			results = append(results, CheckResult{
				Level:   LevelError,
				Zone:    z.ID,
				Message: fmt.Sprintf("polygon zone has only %d vertices (need >= 3)", len(z.Polygon)),
			})
		}

		// Zone out of map bounds (fully outside).
		if md.Width > 0 && md.Height > 0 {
			if z.X+z.W < 0 || z.Y+z.H < 0 || z.X > float32(md.Width) || z.Y > float32(md.Height) {
				results = append(results, CheckResult{
					Level:   LevelWarning,
					Zone:    z.ID,
					Message: fmt.Sprintf("zone is entirely outside map bounds (%dx%d)", md.Width, md.Height),
				})
			}
		}

		// Mobile zone must be a circle.
		if z.Mobility == "mobile" && z.Shape != ShapeCircle {
			results = append(results, CheckResult{
				Level:   LevelError,
				Zone:    z.ID,
				Message: "mobile zone must be a circle (ellipse with width == height)",
			})
		}

		// Unknown zone_type (warning only).
		if z.ZoneType != "" && !knownZoneTypes[z.ZoneType] {
			results = append(results, CheckResult{
				Level:   LevelWarning,
				Zone:    z.ID,
				Message: fmt.Sprintf("unknown zone_type %q (not interpreted by kernel)", z.ZoneType),
			})
		}

		// Exclusive zone should have a zone_type.
		if z.IsExclusive && z.ZoneType == "" {
			results = append(results, CheckResult{
				Level:   LevelWarning,
				Zone:    z.ID,
				Message: "is_exclusive=true but no zone_type set — extensions may not know how to handle exclusivity",
			})
		}
	}

	// --- Spawn point check ---
	if md.Width > 0 && md.Height > 0 && md.Collision != nil {
		sx, sy := md.FindSpawn()
		if md.IsBlocked(int(sx+0.5), int(sy+0.5)) {
			results = append(results, CheckResult{
				Level:   LevelWarning,
				Message: fmt.Sprintf("spawn point (%.0f, %.0f) is on a blocked tile — players may be stuck", sx, sy),
			})
		}
	}

	// --- Spawn zone walkable-tile check ---
	// A spawn zone with no walkable tiles is almost certainly a map-authoring
	// mistake; FindSpawnPoint falls back to FindSpawn() at runtime, so this
	// is a warning, not an error.
	if md.Width > 0 && md.Height > 0 && md.Collision != nil {
		for _, z := range md.SpawnZones {
			if len(walkableTilesInZone(md, z)) == 0 {
				results = append(results, CheckResult{
					Level:   LevelWarning,
					Zone:    z.ID,
					Message: "spawn zone contains no walkable tiles; players will fall back to map-center spawn",
				})
			}
		}
	}

	return results
}

// LogIntegrityResults logs all check results at the appropriate level.
func LogIntegrityResults(logger *slog.Logger, results []CheckResult, mapID string) {
	if len(results) == 0 {
		logger.Info("map integrity check passed", "map", mapID, "issues", 0)
		return
	}

	errors, warnings, infos := 0, 0, 0
	for _, r := range results {
		switch r.Level {
		case LevelError:
			errors++
			logger.Error("map integrity", "map", mapID, "result", r.String())
		case LevelWarning:
			warnings++
			logger.Warn("map integrity", "map", mapID, "result", r.String())
		case LevelInfo:
			infos++
			logger.Info("map integrity", "map", mapID, "result", r.String())
		}
	}
	logger.Info("map integrity check complete",
		"map", mapID, "errors", errors, "warnings", warnings, "infos", infos)
}
