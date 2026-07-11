package worldsim

import (
	"encoding/json"
	"sort"

	"github.com/nats-io/nats.go"
)

// zoneMeta is the JSON representation of a zone sent to extensions via NATS.
// Extensions use this to determine which zones they care about (wall zones,
// A/V-enabled zones, etc.) without reading PocketBase directly.
type zoneMeta struct {
	ID                 string `json:"id"`
	ZoneType           string `json:"zone_type"`
	AvEnabled          bool   `json:"av_enabled"`
	IsExclusive        bool   `json:"is_exclusive"`
	Mobility           string `json:"mobility"`
	PortalTargetMap    string `json:"portal_target_map,omitempty"`
	PortalTargetEntity string `json:"portal_target_entity,omitempty"`
}

// zoneMetadataMsg is the payload published on worldsim.zones and returned by
// worldsim.zones.get.
type zoneMetadataMsg struct {
	Maps map[string][]zoneMeta `json:"maps"`
}

// buildZoneMetadata serializes all zones from all loaded maps into a
// zoneMetadataMsg JSON payload. Caller must NOT hold s.mu.
func (s *Simulator) buildZoneMetadata() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()

	msg := zoneMetadataMsg{Maps: make(map[string][]zoneMeta, len(s.zones))}
	for mapName, zr := range s.zones {
		zones := zr.AllZones()
		metas := make([]zoneMeta, 0, len(zones))
		for _, z := range zones {
			metas = append(metas, zoneMeta{
				ID:                 z.ID,
				ZoneType:           z.ZoneType,
				AvEnabled:          z.AvEnabled,
				IsExclusive:        z.IsExclusive,
				Mobility:           z.Mobility,
				PortalTargetMap:    z.PortalTargetMap,
				PortalTargetEntity: z.PortalTargetEntity,
			})
		}
		sort.Slice(metas, func(i, j int) bool { return metas[i].ID < metas[j].ID })
		msg.Maps[mapName] = metas
	}
	data, err := json.Marshal(msg)
	if err != nil {
		s.logger.Warn("marshal zone metadata", "err", err)
		return nil
	}
	return data
}

// broadcastZoneMetadata publishes the current zone metadata for all maps on
// the worldsim.zones NATS subject. Extensions subscribe to this to receive
// zone updates (e.g. after a map reload) without reading PocketBase directly.
func (s *Simulator) broadcastZoneMetadata() {
	data := s.buildZoneMetadata()
	if data == nil {
		return
	}
	if err := s.nc.Publish("worldsim.zones", data); err != nil {
		s.logger.Warn("publish worldsim.zones", "err", err)
	}
}

// subscribeZoneMetadata sets up the worldsim.zones.get request-reply handler
// so extensions can fetch zone metadata on demand (e.g. on startup or
// reconnect when they missed the initial broadcast).
func (s *Simulator) subscribeZoneMetadata() error {
	if _, err := s.nc.Subscribe("worldsim.zones.get", func(msg *nats.Msg) {
		data := s.buildZoneMetadata()
		if data == nil {
			return
		}
		if err := msg.Respond(data); err != nil {
			s.logger.Warn("respond worldsim.zones.get", "err", err)
		}
	}); err != nil {
		return err
	}
	return nil
}
