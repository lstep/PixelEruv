// ext-av is a first-party extension that bridges zone and proximity events
// to LiveKit. It mints LiveKit tokens and publishes them to clients via
// client.<id>.av_token NATS subjects (forwarded by the pusher as
// AvTokenFrame). It reads the Tiled map from PocketBase to find zones with
// the av_enabled property.
//
// Subscriptions:
//   - zone.enter / zone.exit: A/V-enabled zone rooms
//   - proximity.join / proximity.leave: ad-hoc proximity rooms
//   - map.updated: refresh A/V zone set
//   - worldsim.ready: register on startup
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/livekit/protocol/auth"
	"github.com/nats-io/nats.go"
)

type registerMsg struct {
	ExtensionID        string `json:"extension_id"`
	HeartbeatIntervalS int    `json:"heartbeat_interval_s"`
}

type tiledMapJSON struct {
	Layers []struct {
		Name    string `json:"name"`
		Type    string `json:"type"`
		Objects []struct {
			Name       string `json:"name"`
			Properties []struct {
				Name  string      `json:"name"`
				Value interface{} `json:"value"`
			} `json:"properties"`
		} `json:"objects"`
	} `json:"layers"`
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
	natsURL := envOr("NATS_URL", "nats://localhost:4222")
	pbURL := envOr("POCKETBASE_URL", "http://localhost:8090")
	mapID := envOr("MAP_ID", "test-map")
	extID := envOr("EXTENSION_ID", "av")
	livekitURL := envOr("LIVEKIT_URL", "ws://localhost:7880")
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

	// avZones is the set of zone IDs with av_enabled=true. Updated on
	// startup and on map.updated.
	var avZones []string

	refreshAVZones := func() {
		zones, err := findAVZones(pbURL, mapID, logger)
		if err != nil {
			logger.Warn("find A/V zones failed", "err", err)
			return
		}
		avZones = zones
		logger.Info("found A/V zones", "count", len(avZones), "zones", avZones)
	}

	isAVZone := func(zoneID string) bool {
		for _, z := range avZones {
			if z == zoneID {
				return true
			}
		}
		return false
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
			URL:    livekitURL,
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
			URL:     livekitURL,
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

	// --- Subscribe to map.updated ---
	nc.Subscribe("map.updated", func(m *nats.Msg) {
		logger.Info("map.updated received, refreshing A/V zones", "map", string(m.Data))
		refreshAVZones()
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
		logger.Info("worldsim ready, registering", "map", string(m.Data))
		refreshAVZones()
		publishReg()
		select {
		case readyCh <- struct{}{}:
		default:
		}
	})

	// Wait for PocketBase to be up.
	for i := 0; i < 30; i++ {
		_, err := findAVZones(pbURL, mapID, logger)
		if err == nil {
			break
		}
		logger.Warn("waiting for pocketbase", "attempt", i+1, "err", err)
		time.Sleep(time.Second)
	}

	// Wait for worldsim.ready before initial registration.
	select {
	case <-readyCh:
	case <-time.After(10 * time.Second):
		logger.Warn("worldsim.ready not received, registering anyway", "id", extID)
		refreshAVZones()
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
			ticks++
		}
	}
}

// findAVZones reads the Tiled map from PocketBase and returns zone IDs
// that have the av_enabled property set to true.
func findAVZones(pbURL, mapName string, logger *slog.Logger) ([]string, error) {
	pbURL = strings.TrimRight(pbURL, "/")

	resp, err := http.Get(fmt.Sprintf("%s/api/collections/maps/records?filter=(name=\"%s\")&perPage=1", pbURL, mapName))
	if err != nil {
		return nil, fmt.Errorf("fetch map record: %w", err)
	}
	defer resp.Body.Close()
	var record struct {
		Items []struct {
			ID           string `json:"id"`
			CollectionID string `json:"collectionId"`
			TiledJSON    string `json:"tiled_json"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&record); err != nil {
		return nil, fmt.Errorf("decode map record: %w", err)
	}
	if len(record.Items) == 0 {
		return nil, fmt.Errorf("no map found: %s", mapName)
	}

	r := record.Items[0]
	jsonURL := fmt.Sprintf("%s/api/files/%s/%s/%s", pbURL, r.CollectionID, r.ID, r.TiledJSON)

	jresp, err := http.Get(jsonURL)
	if err != nil {
		return nil, fmt.Errorf("fetch tiled json: %w", err)
	}
	defer jresp.Body.Close()
	body, err := io.ReadAll(jresp.Body)
	if err != nil {
		return nil, fmt.Errorf("read tiled json: %w", err)
	}

	var tiled tiledMapJSON
	if err := json.Unmarshal(body, &tiled); err != nil {
		return nil, fmt.Errorf("parse tiled json: %w", err)
	}

	var avZones []string
	for _, layer := range tiled.Layers {
		if strings.ToLower(layer.Name) != "zones" || layer.Type != "objectgroup" {
			continue
		}
		for _, obj := range layer.Objects {
			if obj.Name == "" {
				continue
			}
			for _, prop := range obj.Properties {
				if prop.Name == "av_enabled" {
					if b, ok := prop.Value.(bool); ok && b {
						avZones = append(avZones, obj.Name)
					}
				}
			}
		}
	}
	return avZones, nil
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
