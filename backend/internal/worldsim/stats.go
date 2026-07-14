package worldsim

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
)

// statsResponse is the JSON shape returned by the worldsim.stats.get
// request-reply handler. The audit service renders it in the /world page.
type statsResponse struct {
	TickHz        int            `json:"tick_hz"`
	TickCount     uint64         `json:"tick_count"`
	Uptime        string         `json:"uptime"`
	TotalEntities int            `json:"total_entities"`
	TotalPlayers  int            `json:"total_players"`
	Maps          []mapStats     `json:"maps"`
	Players       []playerStats  `json:"players"`
	Extensions    []extStats     `json:"extensions"`
}

type mapStats struct {
	Name         string      `json:"name"`
	Width        int         `json:"width"`
	Height       int         `json:"height"`
	PlayerCount  int         `json:"player_count"`
	EntityCount  int         `json:"entity_count"`
	ZoneCount    int         `json:"zone_count"`
	SpawnZones   int         `json:"spawn_zones"`
	Zones        []zoneStats `json:"zones"`
}

type zoneStats struct {
	ID           string `json:"id"`
	ZoneType     string `json:"zone_type"`
	Shape        string `json:"shape"`
	AvEnabled    bool   `json:"av_enabled"`
	IsExclusive  bool   `json:"is_exclusive"`
	PortalTarget string `json:"portal_target,omitempty"`
	Occupancy    int    `json:"occupancy"`
}

type playerStats struct {
	EntityID    string  `json:"entity_id"`
	ClientID    string  `json:"client_id"`
	DisplayName string  `json:"display_name"`
	MapID       string  `json:"map_id"`
	X           float32 `json:"x"`
	Y           float32 `json:"y"`
	IsAdmin     bool    `json:"is_admin"`
	IsGuest     bool    `json:"is_guest"`
	IP          string  `json:"ip,omitempty"`
}

type extStats struct {
	ID               string `json:"id"`
	HeartbeatAge     string `json:"heartbeat_age"`
	Alive            bool   `json:"alive"`
	InputTriggers    int    `json:"input_triggers"`
	GateTriggers     int    `json:"gate_triggers"`
}

// subscribeStats sets up the worldsim.stats.get request-reply handler.
// The audit service calls this to render the /world status page.
func (s *Simulator) subscribeStats() error {
	if _, err := s.nc.Subscribe("worldsim.stats.get", func(msg *nats.Msg) {
		data, err := s.buildStatsResponse()
		if err != nil {
			s.logger.Warn("build stats response", "err", err)
			return
		}
		if err := msg.Respond(data); err != nil {
			s.logger.Warn("respond worldsim.stats.get", "err", err)
		}
	}); err != nil {
		return fmt.Errorf("subscribe worldsim.stats.get: %w", err)
	}
	return nil
}

func (s *Simulator) buildStatsResponse() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()

	// Count entities per map and collect zone occupancy.
	mapPlayers := make(map[string]int)
	mapEntities := make(map[string]int)
	zoneOccupancy := make(map[string]map[string]int) // mapID -> zoneID -> count

	for _, e := range s.entities {
		mapID := "main"
		if e.Position != nil {
			mapID = e.Position.MapId
		}
		mapEntities[mapID]++
	}

	for _, e := range s.clients {
		mapID := "main"
		if e.Position != nil {
			mapID = e.Position.MapId
		}
		mapPlayers[mapID]++
	}

	// Zone occupancy: count players in each zone.
	for _, e := range s.clients {
		mapID := "main"
		if e.Position != nil {
			mapID = e.Position.MapId
		}
		if e.currentZones == nil {
			continue
		}
		if _, ok := zoneOccupancy[mapID]; !ok {
			zoneOccupancy[mapID] = make(map[string]int)
		}
		for zoneID := range e.currentZones {
			zoneOccupancy[mapID][zoneID]++
		}
	}

	// Build map stats.
	var maps []mapStats
	for mapName, md := range s.maps {
		var zones []zoneStats
		for _, z := range md.Zones {
			shapeStr := "rect"
			switch z.Shape {
			case ShapeCircle:
				shapeStr = "circle"
			case ShapePolygon:
				shapeStr = "polygon"
			}
			zs := zoneStats{
				ID:          z.ID,
				ZoneType:    z.ZoneType,
				Shape:       shapeStr,
				AvEnabled:   z.AvEnabled,
				IsExclusive: z.IsExclusive,
				Occupancy:   zoneOccupancy[mapName][z.ID],
			}
			if z.PortalTargetMap != "" {
				zs.PortalTarget = z.PortalTargetMap
			}
			zones = append(zones, zs)
		}
		maps = append(maps, mapStats{
			Name:        mapName,
			Width:       md.Width,
			Height:      md.Height,
			PlayerCount: mapPlayers[mapName],
			EntityCount: mapEntities[mapName],
			ZoneCount:   len(md.Zones),
			SpawnZones:  len(md.SpawnZones),
			Zones:       zones,
		})
	}

	// Build player list.
	var players []playerStats
	for _, e := range s.clients {
		ps := playerStats{
			EntityID:    e.ID,
			ClientID:    e.NetworkSession.ClientID,
			DisplayName: e.DisplayName,
			IsAdmin:     e.IsAdmin,
			IsGuest:     e.IsGuest,
			IP:          e.IP,
		}
		if e.Position != nil {
			ps.MapID = e.Position.MapId
			ps.X = e.Position.X
			ps.Y = e.Position.Y
		}
		players = append(players, ps)
	}

	// Build extension list.
	var exts []extStats
	if s.extMgr != nil {
		s.extMgr.mu.Lock()
		for id, ext := range s.extMgr.extensions {
			age := now.Sub(ext.LastHeartbeat)
			alive := age < ext.HeartbeatInterval*3
			inputCount := 0
			for _, exts := range s.extMgr.inputTriggers {
				if exts[id] {
					inputCount++
				}
			}
			gateCount := 0
			for _, gt := range s.extMgr.gateTriggers {
				if gt.ExtensionID == id {
					gateCount++
				}
			}
			exts = append(exts, extStats{
				ID:            id,
				HeartbeatAge:  age.Round(time.Second).String(),
				Alive:         alive,
				InputTriggers: inputCount,
				GateTriggers:  gateCount,
			})
		}
		s.extMgr.mu.Unlock()
	}

	resp := statsResponse{
		TickHz:        s.tickHz,
		TickCount:     s.tickCount,
		Uptime:        now.Sub(s.startTime).Round(time.Second).String(),
		TotalEntities: len(s.entities),
		TotalPlayers:  len(s.clients),
		Maps:          maps,
		Players:       players,
		Extensions:    exts,
	}
	return json.Marshal(resp)
}
