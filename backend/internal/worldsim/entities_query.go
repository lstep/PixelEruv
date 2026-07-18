package worldsim

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/nats-io/nats.go"
)

// entitiesQueryRequest is the JSON filter for worldsim.entities.query. All
// fields are optional; empty filter returns up to `limit` entities across all
// maps. Cap is enforced server-side (maxEntitiesQueryLimit).
type entitiesQueryRequest struct {
	MapID          string `json:"map_id,omitempty"`
	EntityType     string `json:"entity_type,omitempty"`
	OwnerExtension string `json:"owner_extension,omitempty"`
	ZoneID         string `json:"zone_id,omitempty"`
	Limit          int    `json:"limit,omitempty"`
}

// maxEntitiesQueryLimit caps the number of entities returned by a single
// worldsim.entities.query call. Clients needing more should paginate (future)
// or use worldsim.stats.get for counts.
const maxEntitiesQueryLimit = 500

// entitySnapshot is the JSON shape returned for each entity by
// worldsim.entities.query and worldsim.entity.get. Mirrors the fields the
// audit /world page already exposes via stats, plus the entity state fields
// extensions read in action dispatches.
type entitySnapshot struct {
	EntityID       string  `json:"entity_id"`
	EntityType     string  `json:"entity_type,omitempty"`
	OwnerExtension string  `json:"owner_extension,omitempty"`
	MapID          string  `json:"map_id"`
	X              float32 `json:"x"`
	Y              float32 `json:"y"`
	IsPlayer       bool    `json:"is_player"`
	DisplayName    string  `json:"display_name,omitempty"`
	IsAdmin        bool    `json:"is_admin,omitempty"`
	IsGuest        bool    `json:"is_guest,omitempty"`
	SpriteBase     string  `json:"sprite_base,omitempty"`
	Status         uint32  `json:"status,omitempty"`
	State          string  `json:"state,omitempty"`
	Gid            uint32  `json:"gid,omitempty"`
	GidOff         uint32  `json:"gid_off,omitempty"`
	GidOn          uint32  `json:"gid_on,omitempty"`
	LightIntensity uint32  `json:"light_intensity,omitempty"`
	LightColor     uint32  `json:"light_color,omitempty"`
	LightRadius    float32 `json:"light_radius,omitempty"`
}

// subscribeEntitiesQuery sets up the worldsim.entities.query and
// worldsim.entity.get request-reply handlers. The MCP server (backend/cmd/mcp)
// uses these to expose entity reads to MCP clients.
func (s *Simulator) subscribeEntitiesQuery() error {
	if _, err := s.nc.Subscribe("worldsim.entities.query", func(msg *nats.Msg) {
		var req entitiesQueryRequest
		if len(msg.Data) > 0 {
			if err := json.Unmarshal(msg.Data, &req); err != nil {
				s.respondEntitiesQueryError(msg, "bad request: "+err.Error())
				return
			}
		}
		if req.Limit <= 0 || req.Limit > maxEntitiesQueryLimit {
			req.Limit = maxEntitiesQueryLimit
		}
		out := s.snapshotEntities(&req)
		data, err := json.Marshal(out)
		if err != nil {
			s.respondEntitiesQueryError(msg, "marshal: "+err.Error())
			return
		}
		if err := msg.Respond(data); err != nil {
			s.logger.Warn("respond worldsim.entities.query", "err", err)
		}
	}); err != nil {
		return fmt.Errorf("subscribe worldsim.entities.query: %w", err)
	}

	if _, err := s.nc.Subscribe("worldsim.entity.get", func(msg *nats.Msg) {
		var req struct {
			EntityID string `json:"entity_id"`
		}
		if err := json.Unmarshal(msg.Data, &req); err != nil {
			s.respondEntitiesQueryError(msg, "bad request: "+err.Error())
			return
		}
		if req.EntityID == "" {
			s.respondEntitiesQueryError(msg, "entity_id is required")
			return
		}
		snap, ok := s.snapshotEntity(req.EntityID)
		if !ok {
			s.respondEntitiesQueryError(msg, "entity not found")
			return
		}
		data, err := json.Marshal(snap)
		if err != nil {
			s.respondEntitiesQueryError(msg, "marshal: "+err.Error())
			return
		}
		if err := msg.Respond(data); err != nil {
			s.logger.Warn("respond worldsim.entity.get", "err", err)
		}
	}); err != nil {
		return fmt.Errorf("subscribe worldsim.entity.get: %w", err)
	}
	return nil
}

func (s *Simulator) respondEntitiesQueryError(msg *nats.Msg, reason string) {
	payload, _ := json.Marshal(map[string]any{"error": reason})
	if err := msg.Respond(payload); err != nil {
		s.logger.Warn("respond entities query error", "err", err)
	}
}

// snapshotEntities returns up to req.Limit entity snapshots matching the
// filter. It snapshots under s.mu, builds the result slice, then releases the
// lock before the caller marshals. Sort is by entity_id for stable output.
func (s *Simulator) snapshotEntities(req *entitiesQueryRequest) []entitySnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()

	// If a zone filter is set, find the map's zone registry and check
	// membership. Zone membership is only tracked for player avatars
	// (e.currentZones), so zone_id filter implicitly limits to players.
	var zoneMember map[string]bool
	if req.ZoneID != "" {
		zoneMember = make(map[string]bool)
		for _, e := range s.clients {
			if e.currentZones[req.ZoneID] {
				zoneMember[e.ID] = true
			}
		}
		if len(zoneMember) == 0 {
			return nil
		}
	}

	out := make([]entitySnapshot, 0, len(s.entities))
	for _, e := range s.entities {
		if len(out) >= req.Limit {
			break
		}
		if req.MapID != "" && (e.Position == nil || e.Position.MapId != req.MapID) {
			continue
		}
		if req.EntityType != "" && e.EntityType != req.EntityType {
			continue
		}
		if req.OwnerExtension != "" && e.OwnerExtension != req.OwnerExtension {
			continue
		}
		if zoneMember != nil && !zoneMember[e.ID] {
			continue
		}
		out = append(out, entityToSnapshot(e))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].EntityID < out[j].EntityID })
	return out
}

// snapshotEntity returns a single entity snapshot by ID.
func (s *Simulator) snapshotEntity(entityID string) (entitySnapshot, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entities[entityID]
	if !ok {
		return entitySnapshot{}, false
	}
	return entityToSnapshot(e), true
}

func entityToSnapshot(e *Entity) entitySnapshot {
	snap := entitySnapshot{
		EntityID:       e.ID,
		EntityType:     e.EntityType,
		OwnerExtension: e.OwnerExtension,
		IsPlayer:       e.NetworkSession != nil,
		DisplayName:    e.DisplayName,
		IsAdmin:        e.IsAdmin,
		IsGuest:        e.IsGuest,
		SpriteBase:     e.SpriteBase,
		Status:         e.Status,
		State:          e.State,
		Gid:            e.Gid,
		GidOff:         e.GidOff,
		GidOn:          e.GidOn,
		LightIntensity: e.LightIntensity,
		LightColor:     e.LightColor,
		LightRadius:    e.LightRadius,
	}
	if e.Position != nil {
		snap.MapID = e.Position.MapId
		snap.X = e.Position.X
		snap.Y = e.Position.Y
	}
	return snap
}
