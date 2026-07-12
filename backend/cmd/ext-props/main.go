// ext-props is a first-party extension demonstrating the input-trigger
// model (see 14-zones-and-interactions.md §3a and
// documentation/plans/2026-07-05-decoration-layers-and-interactive-entities-design.md
// Part C): it registers for the "key:E" input, and toggles any adjacent
// entity it owns (owner_extension=ext-props in the map's "Entities" object
// layer, or entity_type=light_switch as a generic fallback) between "on"
// and "off", replicating the new state and a PlayAnimation event.
//
// It never reads the map itself — the worldsim's InputHandlerSystem already
// includes entity_type/owner_extension for every adjacent entity in the
// dispatch payload, so the extension self-filters from that alone.
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

	"github.com/lstep/pixeleruv/backend/internal/audit"
	"github.com/lstep/pixeleruv/backend/internal/otel"
	"github.com/lstep/pixeleruv/backend/internal/version"
	"github.com/nats-io/nats.go"
)

const (
	extID        = "props"
	inputType    = "key:E"
	entityType   = "light_switch"
	animationOn  = 1
	animationOff = 2
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

// propsOptions holds the current option values for ext-props.
type propsOptions struct {
	InteractionRadius float64 `json:"interaction_radius"`
}

type triggerMsg struct {
	ExtensionID   string `json:"extension_id"`
	InputTriggers []struct {
		Input string `json:"input"`
	} `json:"input_triggers"`
}

type adjacentEntityInfo struct {
	EntityID       string `json:"entity_id"`
	EntityType     string `json:"entity_type,omitempty"`
	OwnerExtension string `json:"owner_extension,omitempty"`
}

type actionDispatchMsg struct {
	EntityID         string               `json:"entity_id"`
	Input            string               `json:"input"`
	AdjacentEntities []adjacentEntityInfo `json:"adjacent_entities"`
}

type actionReplyMsg struct {
	Handled bool `json:"handled"`
	Updates []struct {
		EntityID string `json:"entity_id"`
		State    string `json:"state"`
	} `json:"updates"`
	Animations []struct {
		EntityID    string `json:"entity_id"`
		AnimationID uint32 `json:"animation_id"`
	} `json:"animations"`
}

func main() {
	startTime := time.Now()
	natsURL := envOr("NATS_URL", "nats://localhost:4222")
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

	// Track on/off state per owned entity in memory (lite MVP — a real
	// extension might persist this in JetStream KV).
	var mu sync.Mutex
	states := make(map[string]bool) // entity_id -> is_on
	opts := propsOptions{InteractionRadius: 1.5}

	// Subscribe to extension.props.options for hot-reloadable config.
	nc.Subscribe(fmt.Sprintf("extension.%s.options", extID), func(m *nats.Msg) {
		mu.Lock()
		before := opts
		if err := json.Unmarshal(m.Data, &opts); err != nil {
			logger.Warn("parse options", "err", err)
			opts = before
		} else {
			logger.Info("options updated", "interaction_radius", opts.InteractionRadius)
		}
		mu.Unlock()
	})

	// extension.props.action — dispatched for every "key:E" ActionFrame.
	if _, err := nc.Subscribe("extension."+extID+".action", func(m *nats.Msg) {
		var dispatch actionDispatchMsg
		if err := json.Unmarshal(m.Data, &dispatch); err != nil {
			return
		}

		resp := actionReplyMsg{}
		mu.Lock()
		for _, ent := range dispatch.AdjacentEntities {
			owns := ent.OwnerExtension == extID ||
				(ent.OwnerExtension == "" && ent.EntityType == entityType)
			if !owns {
				continue
			}
			isOn := !states[ent.EntityID]
			states[ent.EntityID] = isOn

			state, animID := "off", uint32(animationOff)
			if isOn {
				state, animID = "on", uint32(animationOn)
			}
			resp.Handled = true
			resp.Updates = append(resp.Updates, struct {
				EntityID string `json:"entity_id"`
				State    string `json:"state"`
			}{EntityID: ent.EntityID, State: state})
			resp.Animations = append(resp.Animations, struct {
				EntityID    string `json:"entity_id"`
				AnimationID uint32 `json:"animation_id"`
			}{EntityID: ent.EntityID, AnimationID: animID})

			logger.Info("toggled prop", "entity", ent.EntityID, "state", state)
			audit.Emit(nc, "props.action_triggered", audit.SeverityInfo,
				audit.Actor{EntityID: ent.EntityID, Extension: extID},
				audit.Details{"input": dispatch.Input, "state": state},
				"")
		}
		mu.Unlock()

		if !resp.Handled {
			logger.Info("no owned adjacent entity", "input", dispatch.Input, "adjacent", len(dispatch.AdjacentEntities))
			return // don't own any adjacent entity — let the kernel time out
		}
		data, _ := json.Marshal(resp)
		if err := m.Respond(data); err != nil {
			logger.Warn("respond", "err", err)
		}
	}); err != nil {
		logger.Error("subscribe action", "err", err)
		os.Exit(1)
	}

	regData, _ := json.Marshal(registerMsg{
		ExtensionID:        extID,
		HeartbeatIntervalS: heartbeatS,
		OptionsSchema: []optionFieldDef{
			{Name: "interaction_radius", Type: "number", Default: json.RawMessage("1.5")},
		},
	})
	trigData, _ := json.Marshal(triggerMsg{
		ExtensionID: extID,
		InputTriggers: []struct {
			Input string `json:"input"`
		}{{Input: inputType}},
	})
	regSubject := "extension." + extID + ".register"
	trigSubject := "extension." + extID + ".register_triggers"
	hbSubject := "extension." + extID + ".heartbeat"

	// publishReg sends the registration + trigger + heartbeat triple.
	publishReg := func() {
		nc.Publish(regSubject, regData)
		nc.Publish(trigSubject, trigData)
		nc.Publish(hbSubject, []byte(extID))
	}

	// worldsim.ready fires when worldsim's subscriptions are live (on startup
	// and on restart). Re-register whenever it fires so we never race the
	// initial publish.
	readyCh := make(chan struct{}, 1)
	nc.Subscribe("worldsim.ready", func(m *nats.Msg) {
		logger.Info("worldsim ready, registering", "map", string(m.Data))
		publishReg()
		select {
		case readyCh <- struct{}{}:
		default:
		}
	})

	// Wait for worldsim.ready before the initial registration. Fall back to
	// registering directly after a timeout (e.g. worldsim was already up and
	// we missed the broadcast on extension restart).
	select {
	case <-readyCh:
	case <-time.After(10 * time.Second):
		logger.Warn("worldsim.ready not received, registering anyway", "id", extID)
		publishReg()
	}
	logger.Info("registered props extension", "input", inputType)

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
			// Re-register every 3rd heartbeat (idempotent on worldsim side).
			if ticks%3 == 0 {
				nc.Publish(regSubject, regData)
				nc.Publish(trigSubject, trigData)
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
