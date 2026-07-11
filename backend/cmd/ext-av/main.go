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
	"github.com/lstep/pixeleruv/backend/internal/version"
	"github.com/nats-io/nats.go"
)

type registerMsg struct {
	ExtensionID        string `json:"extension_id"`
	HeartbeatIntervalS int    `json:"heartbeat_interval_s"`
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

func main() {
	startTime := time.Now()
	natsURL := envOr("NATS_URL", "nats://localhost:4222")
	extID := envOr("EXTENSION_ID", "av")
	livekitURL := envOr("LIVEKIT_URL", "ws://localhost:7880")
	// LIVEKIT_PUBLIC_URL is the URL the browser uses to reach LiveKit.
	// Defaults to LIVEKIT_URL (fine when the browser and ext-av share the
	// same network). In Docker, set this to the host-exposed URL (e.g.
	// ws://localhost:7880) since LIVEKIT_URL is the Docker-internal address.
	livekitPublicURL := envOr("LIVEKIT_PUBLIC_URL", livekitURL)
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

	nc, err := nats.Connect(natsURL,
		nats.Name("ext-"+extID),
		nats.ReconnectWait(2*time.Second),
		nats.MaxReconnects(-1),
	)
	if err != nil {
		logger.Error("nats connect", "err", err)
		os.Exit(1)
	}
	defer nc.Close()

	var mu sync.Mutex
	avZones := make(map[string]bool) // zone_id -> true

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
		if ev.ClientID == "" || !isAVZone(ev.ZoneID) {
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
		logger.Info("zone A/V join", "entity", ev.EntityID, "zone", ev.ZoneID, "room", room)
	})

	nc.Subscribe("zone.exit", func(m *nats.Msg) {
		var ev zoneEventPayload
		if err := json.Unmarshal(m.Data, &ev); err != nil {
			return
		}
		if ev.ClientID == "" || !isAVZone(ev.ZoneID) {
			return
		}
		room := "zone-" + slugify(ev.ZoneID)
		publishAVToken(ev.ClientID, avTokenMsg{
			Action: "leave",
			Room:   room,
		})
		logger.Info("zone A/V leave", "entity", ev.EntityID, "zone", ev.ZoneID, "room", room)
	})

	// --- Subscribe to proximity.join / proximity.leave ---
	nc.Subscribe("proximity.join", func(m *nats.Msg) {
		var ev proximityEventPayload
		if err := json.Unmarshal(m.Data, &ev); err != nil {
			return
		}
		if ev.ClientID == "" {
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
	})

	// --- Subscribe to worldsim.zones for live zone updates (map reload) ---
	nc.Subscribe("worldsim.zones", func(m *nats.Msg) {
		logger.Info("worldsim.zones received, updating A/V zones")
		updateAVZones(m.Data)
	})

	// --- Extension registration protocol ---
	regSubject := fmt.Sprintf("extension.%s.register", extID)
	hbSubject := fmt.Sprintf("extension.%s.heartbeat", extID)

	publishReg := func() {
		regData, _ := json.Marshal(registerMsg{
			ExtensionID:        extID,
			HeartbeatIntervalS: heartbeatS,
		})
		nc.Publish(regSubject, regData)
		nc.Publish(hbSubject, []byte(extID))
	}

	readyCh := make(chan struct{}, 1)
	nc.Subscribe("worldsim.ready", func(m *nats.Msg) {
		logger.Info("worldsim ready, fetching zone metadata", "map", string(m.Data))
		fetchZoneMetadata()
		publishReg()
		select {
		case readyCh <- struct{}{}:
		default:
		}
	})

	// Wait for worldsim.ready before initial registration.
	select {
	case <-readyCh:
	case <-time.After(10 * time.Second):
		logger.Warn("worldsim.ready not received, fetching zone metadata anyway", "id", extID)
		fetchZoneMetadata()
		publishReg()
	}

	// Heartbeat + re-register loop.
	ticker := time.NewTicker(time.Duration(heartbeatS) * time.Second)
	defer ticker.Stop()

	var ticks int
	for {
		select {
		case <-ctx.Done():
			logger.Info("shutting down")
			return
		case <-ticker.C:
			nc.Publish(hbSubject, []byte(extID))
			if ticks%3 == 0 {
				publishReg()
			}
			publishHealth(nc, "ext-"+extID, startTime)
			ticks++
		}
	}
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

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func publishHealth(nc *nats.Conn, service string, startTime time.Time) {
	health := map[string]any{
		"service": service,
		"status":  "OK",
		"version": version.Version,
		"uptime":  time.Since(startTime).Round(time.Second).String(),
		"extras":  map[string]any{},
	}
	data, _ := json.Marshal(health)
	nc.Publish("healthz", data)
}
