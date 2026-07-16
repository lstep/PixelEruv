// ext-walls is a first-party extension that registers block gate triggers
// on wall zones. It receives zone metadata from worldsim via NATS
// (worldsim.zones broadcast + worldsim.zones.get request-reply), finds zones
// with zone_type "wall", and tells the worldsim to block movement into them.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/lstep/pixeleruv/backend/internal/otel"
	"github.com/lstep/pixeleruv/backend/internal/version"
	"github.com/nats-io/nats.go"
)

type registerMsg struct {
	ExtensionID        string           `json:"extension_id"`
	HeartbeatIntervalS int              `json:"heartbeat_interval_s"`
	OptionsSchema      []optionFieldDef `json:"options_schema,omitempty"`
}

// optionFieldDef declares a single configurable option.
type optionFieldDef struct {
	Name    string          `json:"name"`
	Type    string          `json:"type"`
	Default json.RawMessage `json:"default"`
}

// wallsOptions holds the current option values for ext-walls.
type wallsOptions struct {
	Enabled bool `json:"enabled"`
}

type triggerMsg struct {
	ExtensionID   string `json:"extension_id"`
	GateTriggers  []struct {
		ZoneID   string `json:"zone_id"`
		Behavior string `json:"behavior"`
	} `json:"gate_triggers"`
	InputTriggers []struct {
		Input string `json:"input"`
	} `json:"input_triggers"`
}

// Effect is a single action from an entity's interactions list.
type Effect struct {
	Action    string   `json:"action"`
	Payload   string   `json:"payload,omitempty"`
	TargetIDs []string `json:"target_ids"`
}

type adjacentEntityInfo struct {
	EntityID         string              `json:"entity_id"`
	EntityType       string              `json:"entity_type,omitempty"`
	OwnerExtension   string              `json:"owner_extension,omitempty"`
	State            string              `json:"state,omitempty"`
	OnInteractAction string              `json:"on_interact_action,omitempty"`
	Actions          string              `json:"actions,omitempty"`
	Interactions     map[string][]Effect `json:"interactions,omitempty"`
}

type actionDispatchMsg struct {
	EntityID         string               `json:"entity_id"`
	Input            string               `json:"input"`
	AdjacentEntities []adjacentEntityInfo `json:"adjacent_entities"`
	TargetEntityID   string               `json:"target_entity_id,omitempty"`
	ActionID         string               `json:"action_id,omitempty"`
}

type actionReplyMsg struct {
	Handled bool `json:"handled"`
}

// zoneMeta is the zone metadata entry from worldsim.zones.
type zoneMeta struct {
	ID       string `json:"id"`
	ZoneType string `json:"zone_type"`
}

// zoneMetadataMsg is the payload of worldsim.zones / worldsim.zones.get.
type zoneMetadataMsg struct {
	Maps map[string][]zoneMeta `json:"maps"`
}

func main() {
	startTime := time.Now()
	natsURL := envOr("NATS_URL", "nats://localhost:4222")
	extID := "walls"
	heartbeatS := 10

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	logger, otelShutdown, err := otel.Init(ctx, "ext-"+extID)
	if err != nil {
		logger.Error("otel init", "err", err)
		os.Exit(1)
	}
	defer otelShutdown(context.Background())

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

	regSubject := fmt.Sprintf("extension.%s.register", extID)
	trigSubject := fmt.Sprintf("extension.%s.register_triggers", extID)
	hbSubject := fmt.Sprintf("extension.%s.heartbeat", extID)

	var mu sync.Mutex
	wallZones := make(map[string]bool) // zone_id -> true
	opts := wallsOptions{Enabled: true}

	// fetchZoneMetadata requests zone metadata from worldsim via NATS
	// request-reply and updates the local wall zone set.
	fetchZoneMetadata := func() {
		reply, err := nc.Request("worldsim.zones.get", nil, 5*time.Second)
		if err != nil {
			logger.Warn("request zone metadata", "err", err)
			return
		}
		var msg zoneMetadataMsg
		if err := json.Unmarshal(reply.Data, &msg); err != nil {
			logger.Warn("parse zone metadata", "err", err)
			return
		}
		mu.Lock()
		wallZones = make(map[string]bool)
		for _, zones := range msg.Maps {
			for _, z := range zones {
				if z.ZoneType == "wall" {
					wallZones[z.ID] = true
				}
			}
		}
		mu.Unlock()
		logger.Info("fetched zone metadata", "wall_zones", len(wallZones))
	}

	// register publishes register + trigger messages based on the current
	// wall zone set.
	register := func() {
		mu.Lock()
		var gateTriggers []struct {
			ZoneID   string `json:"zone_id"`
			Behavior string `json:"behavior"`
		}
		if opts.Enabled {
			for zid := range wallZones {
				gateTriggers = append(gateTriggers, struct {
					ZoneID   string `json:"zone_id"`
					Behavior string `json:"behavior"`
				}{ZoneID: zid, Behavior: "block"})
			}
		}
		mu.Unlock()

		regData, _ := json.Marshal(registerMsg{
			ExtensionID:        extID,
			HeartbeatIntervalS: heartbeatS,
			OptionsSchema: []optionFieldDef{
				{Name: "enabled", Type: "bool", Default: json.RawMessage("true")},
			},
		})
		trigData, _ := json.Marshal(triggerMsg{
			ExtensionID:  extID,
			GateTriggers: gateTriggers,
			InputTriggers: []struct {
				Input string `json:"input"`
			}{{Input: "key:E"}, {Input: "action:execute"}},
		})

		nc.Publish(regSubject, regData)
		nc.Publish(trigSubject, trigData)
		logger.Info("registered walls extension", "triggers", len(gateTriggers))
	}

	// Subscribe to worldsim.zones for live zone updates (e.g. map reload).
	nc.Subscribe("worldsim.zones", func(m *nats.Msg) {
		var msg zoneMetadataMsg
		if err := json.Unmarshal(m.Data, &msg); err != nil {
			logger.Warn("parse worldsim.zones", "err", err)
			return
		}
		mu.Lock()
		wallZones = make(map[string]bool)
		for _, zones := range msg.Maps {
			for _, z := range zones {
				if z.ZoneType == "wall" {
					wallZones[z.ID] = true
				}
			}
		}
		mu.Unlock()
		logger.Info("zone metadata updated via broadcast", "wall_zones", len(wallZones))
		register()
	})

	// Subscribe to extension.walls.options for hot-reloadable config.
	// The enabled option is read by register() on the next periodic
	// re-register (every 3rd heartbeat). We don't call register() here
	// because that would create a feedback loop: register → PublishOptions
	// → options handler → register → ...
	nc.Subscribe(fmt.Sprintf("extension.%s.options", extID), func(m *nats.Msg) {
		mu.Lock()
		before := opts
		if err := json.Unmarshal(m.Data, &opts); err != nil {
			logger.Warn("parse options", "err", err)
			opts = before
		} else {
			logger.Info("options updated", "enabled", opts.Enabled)
		}
		mu.Unlock()
	})

	// extension.walls.action — dispatched for "key:E" and "action:execute".
	// ext-walls handles the "toggle_wall" action verb: it adds/removes the
	// target zone from the active wall set and re-registers gate triggers.
	// Other action verbs (toggle, set_state, etc.) are silently skipped.
	if _, err := nc.Subscribe(fmt.Sprintf("extension.%s.action", extID), func(m *nats.Msg) {
		var dispatch actionDispatchMsg
		if err := json.Unmarshal(m.Data, &dispatch); err != nil {
			return
		}

		resp := actionReplyMsg{}
		mu.Lock()
		for _, ent := range dispatch.AdjacentEntities {
			// ext-walls does NOT filter by OwnerExtension — a door entity
			// may be owned by props (for state/sprite) but declare
			// toggle_wall effects that walls must handle. We process all
			// adjacent entities and rely on the effects list to determine
			// relevance.
			// Determine which action_id to process: for action:execute, use
			// dispatch.ActionID; for key:E immediate mode, use OnInteractAction.
			actionID := dispatch.ActionID
			if dispatch.Input != "action:execute" {
				actionID = ent.OnInteractAction
			}
			if actionID == "" {
				continue
			}
			effects := ent.Interactions[actionID]
			for _, fx := range effects {
				if fx.Action != "toggle_wall" {
					continue
				}
				for _, zid := range fx.TargetIDs {
					if zid == "" {
						continue
					}
					if wallZones[zid] {
						delete(wallZones, zid)
					} else {
						wallZones[zid] = true
					}
					resp.Handled = true
					logger.Info("toggled wall zone", "zone", zid, "active", wallZones[zid])
				}
			}
		}
		mu.Unlock()

		if resp.Handled {
			register() // re-publish gate triggers with updated zone set
		}
		data, _ := json.Marshal(resp)
		if err := m.Respond(data); err != nil {
			logger.Warn("respond", "err", err)
		}
	}); err != nil {
		logger.Error("subscribe action", "err", err)
		os.Exit(1)
	}

	// worldsim.ready fires when worldsim's subscriptions are live (on startup
	// and on restart). Fetch zone metadata and register.
	readyCh := make(chan struct{}, 1)
	nc.Subscribe("worldsim.ready", func(m *nats.Msg) {
		logger.Info("worldsim ready, fetching zone metadata", "map", string(m.Data))
		fetchZoneMetadata()
		register()
		select {
		case readyCh <- struct{}{}:
		default:
		}
	})

	// Wait for worldsim.ready before the initial registration. Fall back to
	// requesting zone metadata directly after a timeout (e.g. worldsim was
	// already up and we missed the broadcast on extension restart).
	select {
	case <-readyCh:
	case <-time.After(10 * time.Second):
		logger.Warn("worldsim.ready not received, fetching zone metadata anyway", "id", extID)
		fetchZoneMetadata()
		register()
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
			// Re-register every 3rd heartbeat.
			if ticks%3 == 0 {
				register()
			}
			publishHealth(nc, "ext-"+extID, startTime)
			ticks++
		}
	}
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
