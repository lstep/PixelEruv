package worldsim

import (
	"encoding/json"

	"github.com/nats-io/nats.go"
)

// entityInfoMsg is the request payload for worldsim.entity_info.
type entityInfoMsg struct {
	EntityID string `json:"entity_id"`
}

// entityInfoReply is the reply payload for worldsim.entity_info. Empty
// EntityID signals the entity was not found (callers must check).
type entityInfoReply struct {
	EntityID    string `json:"entity_id"`
	IsAdmin     bool   `json:"is_admin"`
	Status      uint32 `json:"status"`
	DisplayName string `json:"display_name"`
	MapID       string `json:"map_id"`
	ClientID    string `json:"client_id,omitempty"`
}

// subscribeEntityInfo sets up the worldsim.entity_info request-reply handler.
// Extensions query it to authorize per-entity actions (e.g. ext-rec checks
// is_admin before starting a recording) without reading PocketBase directly.
// Mirrors the worldsim.zones.get / worldsim.stats.get request-reply pattern.
func (s *Simulator) subscribeEntityInfo() error {
	if _, err := s.nc.Subscribe("worldsim.entity_info", func(msg *nats.Msg) {
		var req entityInfoMsg
		if err := json.Unmarshal(msg.Data, &req); err != nil {
			s.logger.Warn("entity_info unmarshal", "err", err)
			return
		}
		s.mu.Lock()
		e, ok := s.entities[req.EntityID]
		s.mu.Unlock()
		if !ok {
			// Reply with an empty record so the caller can distinguish
			// "not found" from a transport error.
			if err := msg.Respond([]byte(`{"entity_id":""}`)); err != nil {
				s.logger.Warn("respond worldsim.entity_info (not found)", "err", err)
			}
			return
		}
		reply := entityInfoReply{
			EntityID:    e.ID,
			IsAdmin:     e.IsAdmin,
			Status:      e.Status,
			DisplayName: e.DisplayName,
			MapID:       e.Position.GetMapId(),
		}
		if e.NetworkSession != nil {
			reply.ClientID = e.NetworkSession.ClientID
		}
		data, err := json.Marshal(reply)
		if err != nil {
			s.logger.Warn("marshal entity_info reply", "err", err)
			return
		}
		if err := msg.Respond(data); err != nil {
			s.logger.Warn("respond worldsim.entity_info", "err", err)
		}
	}); err != nil {
		return err
	}
	return nil
}
