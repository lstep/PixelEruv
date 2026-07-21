package worldsim

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log/slog"
	"sort"

	"github.com/nats-io/nats.go"
)

// ProximitySystem groups nearby players (not in av_enabled zones) into
// proximity A/V groups via connected components on the "who is near whom"
// graph, then publishes edge-triggered proximity.join/proximity.leave events
// when a player's group assignment changes.
//
// Field ownership (writes):
//   - Entity.currentProximityGroup
type ProximitySystem struct {
	sink   ProximitySink
	logger *slog.Logger
}

// NewProximitySystem constructs a ProximitySystem. sink receives
// proximity.join/proximity.leave events; logger is used for publish errors.
func NewProximitySystem(sink ProximitySink, logger *slog.Logger) *ProximitySystem {
	return &ProximitySystem{sink: sink, logger: logger}
}

// Step runs proximity clustering. It is throttled by the caller (tick runs
// it every 5th tick). Caller must hold s.mu.
func (p *ProximitySystem) Step(ctx context.Context, w *World) {
	// Build a set of av_enabled zone IDs across all maps. Players inside
	// these zones get zone-based A/V instead of proximity A/V.
	avZones := make(map[string]bool)
	for _, zr := range w.zones {
		if zr == nil {
			continue
		}
		for _, z := range zr.zones {
			if z.AvEnabled {
				avZones[z.ID] = true
			}
		}
	}

	// Collect player entities not in any av_enabled zone.
	// DND players are excluded entirely: they get no proximity group, so
	// no proximity.join is emitted for them. Toggling to DND naturally
	// produces a proximity.leave on the next tick (their
	// currentProximityGroup is no longer in newGroup), and toggling back
	// to Available re-includes them on the next tick — automatic eject
	// and rejoin without synthetic events.
	var players []*Entity
	inAVZone := make(map[string]bool)
	for _, e := range w.entities {
		if e.NetworkSession == nil {
			continue
		}
		if e.Status == statusDoNotDisturb {
			continue
		}
		for zid := range e.currentZones {
			if avZones[zid] {
				inAVZone[e.ID] = true
				break
			}
		}
		if !inAVZone[e.ID] {
			players = append(players, e)
		}
	}

	// Build adjacency: A and B are adjacent if A is inside B's proximity zone
	// or B is inside A's proximity zone (symmetric). Zone membership is
	// already tracked in currentZones from the zone detection step above.
	adj := make(map[string]map[string]bool)
	for _, e := range players {
		adj[e.ID] = make(map[string]bool)
	}
	for _, a := range players {
		for _, b := range players {
			if a.ID == b.ID {
				continue
			}
			if a.currentZones["prox-"+b.ID] || b.currentZones["prox-"+a.ID] {
				adj[a.ID][b.ID] = true
				adj[b.ID][a.ID] = true
			}
		}
	}

	// Find connected components via BFS.
	visited := make(map[string]bool)
	var groups [][]string
	for _, e := range players {
		if visited[e.ID] {
			continue
		}
		// BFS from this node.
		queue := []string{e.ID}
		visited[e.ID] = true
		var comp []string
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			comp = append(comp, cur)
			for neighbor := range adj[cur] {
				if !visited[neighbor] {
					visited[neighbor] = true
					queue = append(queue, neighbor)
				}
			}
		}
		groups = append(groups, comp)
	}

	// Assign group IDs and detect changes.
	//
	// Group IDs are stable across membership changes: if any member of the
	// connected component already has a currentProximityGroup, that ID is
	// reused. A new ID is minted only for brand-new groups (no member has
	// an existing group). This prevents all existing members from being
	// torn down and re-joined to a new LiveKit room when a player joins or
	// leaves the group — only the joining/leaving player gets an event.
	newGroup := make(map[string]string)       // entity_id -> group_id
	groupMembers := make(map[string][]string) // group_id -> sorted member entity IDs
	for _, comp := range groups {
		if len(comp) < 2 {
			// Singleton — no proximity group.
			continue
		}
		sorted := append([]string(nil), comp...)
		sort.Strings(sorted)
		// Reuse an existing group ID from any member already in a group.
		// This keeps the LiveKit room stable when members join/leave.
		var gid string
		for _, id := range sorted {
			if e, ok := w.entities[id]; ok && e.currentProximityGroup != "" {
				gid = e.currentProximityGroup
				break
			}
		}
		if gid == "" {
			// New group — require all members to be stationary before
			// activating A/V. This prevents A/V thrashing when a player
			// walks past another without stopping. See issue #88.
			allStationary := true
			for _, id := range sorted {
				if e, ok := w.entities[id]; ok && e.stationaryTicks < proximityStationaryThreshold {
					allStationary = false
					break
				}
			}
			if !allStationary {
				continue
			}
			// Brand-new group: mint a new ID from the member hash.
			h := fnv.New64a()
			for _, id := range sorted {
				h.Write([]byte(id))
				h.Write([]byte{0}) // separator
			}
			gid = fmt.Sprintf("proxgroup-%016x", h.Sum64())
		}
		for _, id := range comp {
			newGroup[id] = gid
		}
		groupMembers[gid] = sorted
	}

	// Publish edge-triggered events for changes.
	for _, e := range players {
		old := e.currentProximityGroup
		newG := newGroup[e.ID]

		if old == newG {
			continue
		}

		// Suppress join for moving players joining an existing group from
		// scratch. New groups are already filtered above. Don't update
		// currentProximityGroup — next cycle retries. See issue #88.
		if old == "" && newG != "" && e.stationaryTicks < proximityStationaryThreshold {
			continue
		}

		clientID := e.NetworkSession.ClientID

		// Leave old group (if any).
		if old != "" {
			p.sink.PublishProximityEvent(ctx, "proximity.leave", e.ID, clientID, old, e.Position.MapId, nil)
		}

		// Join new group (if any).
		if newG != "" {
			p.sink.PublishProximityEvent(ctx, "proximity.join", e.ID, clientID, newG, e.Position.MapId, groupMembers[newG])
		}

		e.currentProximityGroup = newG
	}

	// Players in av_enabled zones leave any proximity group they were in.
	for _, e := range w.entities {
		if e.NetworkSession == nil || !inAVZone[e.ID] {
			continue
		}
		if e.currentProximityGroup != "" {
			clientID := e.NetworkSession.ClientID
			p.sink.PublishProximityEvent(ctx, "proximity.leave", e.ID, clientID, e.currentProximityGroup, e.Position.MapId, nil)
			e.currentProximityGroup = ""
		}
	}
}

// proximityEventPayload is the NATS payload for proximity.join/leave events.
type proximityEventPayload struct {
	EntityID string   `json:"entity_id"`
	ClientID string   `json:"client_id"`
	GroupID  string   `json:"group_id"`
	MapID    string   `json:"map_id"`
	Members  []string `json:"members,omitempty"`
}

// natProximitySink is the production ProximitySink, publishing proximity
// events to NATS.
type natProximitySink struct {
	nc     *nats.Conn
	logger *slog.Logger
}

// NewNatProximitySink constructs a production ProximitySink backed by NATS.
func NewNatProximitySink(nc *nats.Conn, logger *slog.Logger) ProximitySink {
	return &natProximitySink{nc: nc, logger: logger}
}

func (s *natProximitySink) PublishProximityEvent(ctx context.Context, event, entityID, clientID, groupID, mapID string, members []string) {
	payload := proximityEventPayload{
		EntityID: entityID,
		ClientID: clientID,
		GroupID:  groupID,
		MapID:    mapID,
		Members:  members,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		s.logger.WarnContext(ctx, "proximity event marshal", "err", err)
		return
	}
	if err := s.nc.Publish(event, data); err != nil {
		s.logger.WarnContext(ctx, "proximity event publish", "event", event, "err", err)
	}
	s.logger.InfoContext(ctx, "proximity event", "event", event, "entity", entityID, "group", groupID)
}
