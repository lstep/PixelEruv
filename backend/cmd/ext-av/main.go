// ext-av is a first-party extension that bridges zone and proximity events
// to LiveKit. It mints LiveKit tokens and publishes them to clients via
// client.<id>.av_token NATS subjects (forwarded by the pusher as
// AvTokenFrame). It receives zone metadata from worldsim via NATS
// (worldsim.zones broadcast + worldsim.zones.get request-reply) to find
// zones with the av_enabled property.
//
// Subscriptions:
//   - zone.enter / zone.exit: A/V-enabled zone rooms
//   - proximity.join / proximity.leave: ad-hoc proximity rooms
//   - worldsim.zones: live zone metadata updates (map reload)
//   - worldsim.ready: fetch zone metadata + register on startup
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/livekit/protocol/auth"
	"github.com/lstep/pixeleruv/backend/internal/audit"
	"github.com/lstep/pixeleruv/backend/internal/extkit"
	"github.com/lstep/pixeleruv/backend/internal/otel"
	"github.com/nats-io/nats.go"
)

// avOptions holds the current option values for ext-av.
type avOptions struct {
	ProximityAudioEnabled bool `json:"proximity_audio_enabled"`
	ZoneAudioEnabled      bool `json:"zone_audio_enabled"`
}

// zoneMeta is the zone metadata entry from worldsim.zones.
type zoneMeta struct {
	ID        string `json:"id"`
	AvEnabled bool   `json:"av_enabled"`
}

// zoneMetadataMsg is the payload of worldsim.zones / worldsim.zones.get.
type zoneMetadataMsg struct {
	Maps map[string][]zoneMeta `json:"maps"`
}

// zoneEventPayload is the NATS payload for zone.enter/zone.exit.
type zoneEventPayload struct {
	EntityID string `json:"entity_id"`
	ClientID string `json:"client_id"`
	ZoneID   string `json:"zone_id"`
	MapID    string `json:"map_id"`
}

// proximityEventPayload is the NATS payload for proximity.join/leave.
type proximityEventPayload struct {
	EntityID string   `json:"entity_id"`
	ClientID string   `json:"client_id"`
	GroupID  string   `json:"group_id"`
	MapID    string   `json:"map_id"`
	Members  []string `json:"members"`
}

// avTokenMsg is the NATS payload published on client.<id>.av_token.
type avTokenMsg struct {
	Action  string   `json:"action"` // "join" or "leave"
	Room    string   `json:"room"`
	Token   string   `json:"token,omitempty"`
	URL     string   `json:"url,omitempty"`
	Members []string `json:"members,omitempty"`
}

// Player presence status values, matching worldsim's status constants and the
// DisplayName.status proto field. DND fully excludes the player from A/V.
const statusDoNotDisturb = 2

// playerStatusMsg is the NATS payload of worldsim.player_status.
type playerStatusMsg struct {
	EntityID string `json:"entity_id"`
	Status   uint32 `json:"status"`
}

func main() {
	natsURL := extkit.EnvOr("NATS_URL", "nats://localhost:4222")
	extID := extkit.EnvOr("EXTENSION_ID", "av")
	livekitURL := extkit.EnvOr("LIVEKIT_URL", "ws://localhost:7880")
	// LIVEKIT_PUBLIC_URL is the URL the browser uses to reach LiveKit.
	// Defaults to LIVEKIT_URL (fine when the browser and ext-av share the
	// same network). In Docker, set this to the host-exposed URL (e.g.
	// ws://localhost:7880) since LIVEKIT_URL is the Docker-internal address.
	livekitPublicURL := extkit.EnvOr("LIVEKIT_PUBLIC_URL", livekitURL)
	livekitAPIKey := os.Getenv("LIVEKIT_API_KEY")
	livekitAPISecret := os.Getenv("LIVEKIT_API_SECRET")
	heartbeatS := 10

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	if livekitAPIKey == "" || livekitAPISecret == "" {
		logger.Error("LIVEKIT_API_KEY and LIVEKIT_API_SECRET must be set")
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	logger, otelShutdown, err := otel.Init(ctx, "ext-"+extID)
	if err != nil {
		logger.Error("otel init", "err", err)
		os.Exit(1)
	}
	defer otelShutdown(context.Background())

	nc, err := extkit.ConnectNATS(natsURL, extID)
	if err != nil {
		logger.Error("nats connect", "err", err)
		os.Exit(1)
	}
	defer nc.Close()

	var mu sync.Mutex
	avZones := make(map[string]bool) // zone_id -> true
	opts := avOptions{ProximityAudioEnabled: true, ZoneAudioEnabled: true}

	// zoneRoomState records the room + clientID needed to emit a proactive
	// leave when a player toggles to DND while in a zone A/V room.
	type zoneRoomState struct {
		Room     string
		ClientID string
	}
	// playerStatus tracks each player's presence status (0=Available,
	// 1=Busy, 2=DND), updated via worldsim.player_status. Used to skip
	// token minting for DND players (fully excluded from A/V). A missing
	// entry defaults to Available. Lost on ext-av restart; players
	// re-broadcast on toggle, so only DND players are misclassified as
	// Available until they re-toggle (acceptable v1).
	playerStatus := make(map[string]uint32)
	// activeZoneRoom tracks the LiveKit zone room a player currently holds
	// a join token for, so a toggle to DND can proactively eject them
	// (emit a leave token) without waiting for a zone.exit event.
	activeZoneRoom := make(map[string]zoneRoomState)

	// isDND reports whether the player is currently Do Not Disturb.
	isDND := func(entityID string) bool {
		mu.Lock()
		defer mu.Unlock()
		return playerStatus[entityID] == statusDoNotDisturb
	}

	// updateAVZones parses a zoneMetadataMsg and updates the local A/V zone set.
	updateAVZones := func(data []byte) {
		var msg zoneMetadataMsg
		if err := json.Unmarshal(data, &msg); err != nil {
			logger.Warn("parse zone metadata", "err", err)
			return
		}
		mu.Lock()
		avZones = make(map[string]bool)
		for _, zones := range msg.Maps {
			for _, z := range zones {
				if z.AvEnabled {
					avZones[z.ID] = true
				}
			}
		}
		mu.Unlock()
		logger.Info("A/V zones updated", "count", len(avZones))
	}

	isAVZone := func(zoneID string) bool {
		mu.Lock()
		defer mu.Unlock()
		return avZones[zoneID]
	}

	// fetchZoneMetadata requests zone metadata from worldsim via NATS
	// request-reply.
	fetchZoneMetadata := func() {
		reply, err := nc.Request("worldsim.zones.get", nil, 5*time.Second)
		if err != nil {
			logger.Warn("request zone metadata", "err", err)
			return
		}
		updateAVZones(reply.Data)
	}

	// mintToken creates a LiveKit JWT for the given room and identity.
	mintToken := func(room, identity string) (string, error) {
		at := auth.NewAccessToken(livekitAPIKey, livekitAPISecret)
		grant := &auth.VideoGrant{
			RoomJoin: true,
			Room:     room,
		}
		at.AddGrant(grant).
			SetIdentity(identity).
			SetValidFor(1 * time.Hour)
		return at.ToJWT()
	}

	// publishAVToken publishes a client.<id>.av_token message.
	publishAVToken := func(clientID string, msg avTokenMsg) {
		if clientID == "" {
			return
		}
		data, _ := json.Marshal(msg)
		subject := fmt.Sprintf("client.%s.av_token", clientID)
		if err := nc.Publish(subject, data); err != nil {
			logger.Warn("av_token publish", "client", clientID, "err", err)
		}
	}

	// --- Subscribe to zone.enter / zone.exit ---
	nc.Subscribe("zone.enter", func(m *nats.Msg) {
		var ev zoneEventPayload
		if err := json.Unmarshal(m.Data, &ev); err != nil {
			return
		}
		mu.Lock()
		zoneEnabled := opts.ZoneAudioEnabled
		mu.Unlock()
		if ev.ClientID == "" || !isAVZone(ev.ZoneID) || !zoneEnabled {
			return
		}
		// DND players are fully excluded from A/V — skip token minting.
		if isDND(ev.EntityID) {
			return
		}
		room := "zone-" + slugify(ev.ZoneID)
		token, err := mintToken(room, ev.EntityID)
		if err != nil {
			logger.Warn("mint token", "err", err, "room", room)
			return
		}
		publishAVToken(ev.ClientID, avTokenMsg{
			Action: "join",
			Room:   room,
			Token:  token,
			URL:    livekitPublicURL,
		})
		mu.Lock()
		activeZoneRoom[ev.EntityID] = zoneRoomState{Room: room, ClientID: ev.ClientID}
		mu.Unlock()
		logger.Info("zone A/V join", "entity", ev.EntityID, "zone", ev.ZoneID, "room", room)
		audit.Emit(nc, "av.token_minted", audit.SeverityInfo,
			audit.Actor{EntityID: ev.EntityID, ClientID: ev.ClientID, Extension: "av"},
			audit.Details{"source": "zone", "room": room, "zone": ev.ZoneID},
			"")
	})

	nc.Subscribe("zone.exit", func(m *nats.Msg) {
		var ev zoneEventPayload
		if err := json.Unmarshal(m.Data, &ev); err != nil {
			return
		}
		mu.Lock()
		zoneEnabled := opts.ZoneAudioEnabled
		mu.Unlock()
		if ev.ClientID == "" || !isAVZone(ev.ZoneID) || !zoneEnabled {
			return
		}
		room := "zone-" + slugify(ev.ZoneID)
		publishAVToken(ev.ClientID, avTokenMsg{
			Action: "leave",
			Room:   room,
		})
		mu.Lock()
		delete(activeZoneRoom, ev.EntityID)
		mu.Unlock()
		logger.Info("zone A/V leave", "entity", ev.EntityID, "zone", ev.ZoneID, "room", room)
		audit.Emit(nc, "av.token_revoked", audit.SeverityInfo,
			audit.Actor{EntityID: ev.EntityID, ClientID: ev.ClientID, Extension: "av"},
			audit.Details{"source": "zone", "room": room, "zone": ev.ZoneID},
			"")
	})

	// --- Subscribe to proximity.join / proximity.leave ---
	nc.Subscribe("proximity.join", func(m *nats.Msg) {
		var ev proximityEventPayload
		if err := json.Unmarshal(m.Data, &ev); err != nil {
			return
		}
		mu.Lock()
		proxEnabled := opts.ProximityAudioEnabled
		mu.Unlock()
		if ev.ClientID == "" || !proxEnabled {
			return
		}
		room := ev.GroupID // already "proxgroup-<hash>"
		token, err := mintToken(room, ev.EntityID)
		if err != nil {
			logger.Warn("mint token", "err", err, "room", room)
			return
		}
		publishAVToken(ev.ClientID, avTokenMsg{
			Action:  "join",
			Room:    room,
			Token:   token,
			URL:     livekitPublicURL,
			Members: ev.Members,
		})
		logger.Info("proximity A/V join", "entity", ev.EntityID, "group", ev.GroupID, "members", ev.Members)
		audit.Emit(nc, "av.token_minted", audit.SeverityInfo,
			audit.Actor{EntityID: ev.EntityID, ClientID: ev.ClientID, Extension: "av"},
			audit.Details{"source": "proximity", "room": ev.GroupID, "members": ev.Members},
			"")
	})

	nc.Subscribe("proximity.leave", func(m *nats.Msg) {
		var ev proximityEventPayload
		if err := json.Unmarshal(m.Data, &ev); err != nil {
			return
		}
		if ev.ClientID == "" {
			return
		}
		publishAVToken(ev.ClientID, avTokenMsg{
			Action: "leave",
			Room:   ev.GroupID,
		})
		logger.Info("proximity A/V leave", "entity", ev.EntityID, "group", ev.GroupID)
		audit.Emit(nc, "av.token_revoked", audit.SeverityInfo,
			audit.Actor{EntityID: ev.EntityID, ClientID: ev.ClientID, Extension: "av"},
			audit.Details{"source": "proximity", "room": ev.GroupID},
			"")
	})

	// --- Subscribe to worldsim.zones for live zone updates (map reload) ---
	nc.Subscribe("worldsim.zones", func(m *nats.Msg) {
		logger.Info("worldsim.zones received, updating A/V zones")
		updateAVZones(m.Data)
	})

	// --- Subscribe to worldsim.player_status for DND A/V exclusion ---
	// On a toggle to DND, proactively eject the player from any active zone
	// A/V room (emit a leave token) so they are fully excluded immediately,
	// without waiting for a zone.exit. Proximity exclusion is handled by
	// worldsim skipping DND players in proximity clustering, which emits
	// proximity.leave on the next tick.
	nc.Subscribe("worldsim.player_status", func(m *nats.Msg) {
		var ev playerStatusMsg
		if err := json.Unmarshal(m.Data, &ev); err != nil {
			logger.Warn("parse player_status", "err", err)
			return
		}
		mu.Lock()
		playerStatus[ev.EntityID] = ev.Status
		var eject zoneRoomState
		hasEject := false
		if ev.Status == statusDoNotDisturb {
			if st, ok := activeZoneRoom[ev.EntityID]; ok {
				eject = st
				hasEject = true
				delete(activeZoneRoom, ev.EntityID)
			}
		}
		mu.Unlock()
		if hasEject {
			publishAVToken(eject.ClientID, avTokenMsg{Action: "leave", Room: eject.Room})
			logger.Info("zone A/V leave (DND)", "entity", ev.EntityID, "room", eject.Room)
			audit.Emit(nc, "av.token_revoked", audit.SeverityInfo,
				audit.Actor{EntityID: ev.EntityID, ClientID: eject.ClientID, Extension: "av"},
				audit.Details{"source": "zone", "room": eject.Room, "reason": "dnd"},
				"")
		}
	})

	// --- Subscribe to extension.av.options for hot-reloadable config ---
	if err := extkit.SubscribeOptions(nc, extID, &opts, &mu, logger, func() {
		logger.Info("options updated", "proximity_audio", opts.ProximityAudioEnabled, "zone_audio", opts.ZoneAudioEnabled)
	}); err != nil {
		logger.Error("subscribe options", "err", err)
		os.Exit(1)
	}

	// --- Extension registration protocol ---
	regSubject := fmt.Sprintf("extension.%s.register", extID)
	hbSubject := fmt.Sprintf("extension.%s.heartbeat", extID)

	regData, _ := json.Marshal(extkit.RegisterMsg{
		ExtensionID:        extID,
		HeartbeatIntervalS: heartbeatS,
		OptionsSchema: []extkit.OptionFieldDef{
			{Name: "proximity_audio_enabled", Type: "bool", Default: json.RawMessage("true")},
			{Name: "zone_audio_enabled", Type: "bool", Default: json.RawMessage("true")},
		},
	})

	// publishReg sends the registration + heartbeat pair (for initial
	// registration on worldsim.ready). The heartbeat loop's re-register
	// callback only publishes the registration since HeartbeatLoop already
	// publishes heartbeats.
	publishReg := func() {
		nc.Publish(regSubject, regData)
		nc.Publish(hbSubject, []byte(extID))
	}

	extkit.WaitForReady(nc, logger, 10*time.Second, func(_ string) {
		fetchZoneMetadata()
		publishReg()
	})

	// Heartbeat + re-register loop. onReRegister publishes only the
	// registration (HeartbeatLoop handles the heartbeat + health publish).
	extkit.HeartbeatLoop(ctx, nc, extID, heartbeatS, func() {
		nc.Publish(regSubject, regData)
	})
	logger.Info("shutting down")
}

// slugify replaces non-alphanumeric characters with hyphens, since LiveKit
// room names have character restrictions.
func slugify(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	return b.String()
}
