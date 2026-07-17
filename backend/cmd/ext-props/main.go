// ext-props is a first-party extension that handles entity state and
// appearance interactions (see
// documentation/plans/2026-07-15-interaction-system-design.md). It
// registers for "key:E" and "action:execute" inputs, reads the
// entity's interactions data from the dispatch payload, and processes
// the effects it knows how to handle (toggle, set_state, activate,
// deactivate, turn_on, turn_off). Effects it doesn't know (toggle_wall,
// send_notification, etc.) are silently skipped — those are handled by
// other extensions.
//
// The extension is a generic interpreter: the entity data declares
// which effects to fire, the extension code decides what each action
// verb does. Adding a new entity with new behavior is a Tiled property
// exercise, not a code change.
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

	"github.com/lstep/pixeleruv/backend/internal/audit"
	"github.com/lstep/pixeleruv/backend/internal/otel"
	"github.com/lstep/pixeleruv/backend/internal/version"
	"github.com/nats-io/nats.go"
)

const (
	extID         = "props"
	inputKeyE     = "key:E"
	inputExecute  = "action:execute"
	animationOn   = 1
	animationOff  = 2
	animationClick = 3
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

// Effect is a single action to apply to a set of targets, declared in
// the entity's "interactions" Tiled property.
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
	Gid              uint32              `json:"gid,omitempty"`
	GidOff           uint32              `json:"gid_off,omitempty"`
	GidOn            uint32              `json:"gid_on,omitempty"`
	OnInteractAction string              `json:"on_interact_action,omitempty"`
	Actions          string              `json:"actions,omitempty"`
	Interactions     map[string][]Effect `json:"interactions,omitempty"`
}

type actionDispatchMsg struct {
	EntityID         string               `json:"entity_id"`
	Input            string               `json:"input"`
	AdjacentEntities []adjacentEntityInfo `json:"adjacent_entities"`
	TargetEntities   []adjacentEntityInfo `json:"target_entities,omitempty"`
	TargetEntityID   string               `json:"target_entity_id,omitempty"`
	ActionID         string               `json:"action_id,omitempty"`
}

type availableAction struct {
	EntityID    string `json:"entity_id"`
	ActionID    string `json:"action_id"`
	Label       string `json:"label"`
	EntityLabel string `json:"entity_label,omitempty"`
}

type actionReplyMsg struct {
	Handled           bool `json:"handled"`
	Updates           []struct {
		EntityID string `json:"entity_id"`
		State    string `json:"state"`
	} `json:"updates,omitempty"`
	AppearanceUpdates []struct {
		EntityID string `json:"entity_id"`
		Gid      uint32 `json:"gid"`
	} `json:"appearance_updates,omitempty"`
	Animations []struct {
		EntityID    string `json:"entity_id"`
		AnimationID uint32 `json:"animation_id"`
	} `json:"animations,omitempty"`
	AvailableActions []availableAction `json:"available_actions,omitempty"`
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

	// extension.props.action — dispatched for "key:E" and "action:execute".
	// The handler reads the entity's interactions data and processes the
	// effects it knows (toggle, set_state, activate, deactivate, turn_on,
	// turn_off). Unknown action verbs are silently skipped.
	if _, err := nc.Subscribe("extension."+extID+".action", func(m *nats.Msg) {
		var dispatch actionDispatchMsg
		if err := json.Unmarshal(m.Data, &dispatch); err != nil {
			return
		}

		resp := actionReplyMsg{}
		mu.Lock()
		for _, ent := range dispatch.AdjacentEntities {
			owns := ent.OwnerExtension == extID
			if !owns {
				continue
			}

			if dispatch.Input == inputExecute {
				// Popup choice: find the target entity and process its
				// interactions[action_id] effects.
				if ent.EntityID != dispatch.TargetEntityID {
					continue
				}
				effects := ent.Interactions[dispatch.ActionID]
				for _, fx := range effects {
					processEffect(fx, &dispatch, &resp, states)
				}
				if resp.Handled {
					resp.Animations = append(resp.Animations, struct {
						EntityID    string `json:"entity_id"`
						AnimationID uint32 `json:"animation_id"`
					}{EntityID: ent.EntityID, AnimationID: animationClick})
				}
			} else {
				// key:E press: check immediate mode vs popup mode.
				if ent.OnInteractAction != "" {
					// Immediate mode: fire the declared action's effects.
					effects := ent.Interactions[ent.OnInteractAction]
					for _, fx := range effects {
						processEffect(fx, &dispatch, &resp, states)
					}
					if resp.Handled {
						resp.Animations = append(resp.Animations, struct {
							EntityID    string `json:"entity_id"`
							AnimationID uint32 `json:"animation_id"`
						}{EntityID: ent.EntityID, AnimationID: animationClick})
					}
				}
				if ent.Actions != "" {
					// Popup mode: return available actions for the client.
					for _, actionID := range splitAndTrim(ent.Actions) {
						label, visible := buildPopupAction(actionID, ent.State)
						if visible {
							resp.AvailableActions = append(resp.AvailableActions, availableAction{
								EntityID:    ent.EntityID,
								ActionID:    actionID,
								Label:       label,
								EntityLabel: ent.EntityType,
							})
							resp.Handled = true
						}
					}
				}
			}

			if resp.Handled {
				logger.Info("processed interaction", "entity", ent.EntityID, "input", dispatch.Input)
				audit.Emit(nc, "props.action_triggered", audit.SeverityInfo,
					audit.Actor{EntityID: ent.EntityID, Extension: extID},
					audit.Details{"input": dispatch.Input},
					"")
			}
		}
		mu.Unlock()

		if !resp.Handled {
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
		}{
			{Input: inputKeyE},
			{Input: inputExecute},
		},
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
	logger.Info("registered props extension", "inputs", []string{inputKeyE, inputExecute})

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

// processEffect handles a single effect from an entity's interactions
// list. ext-props handles action verbs related to entity state and
// appearance. Unknown verbs (toggle_wall, send_notification, etc.) are
// silently skipped — other extensions handle those.
func processEffect(fx Effect, dispatch *actionDispatchMsg, resp *actionReplyMsg, states map[string]bool) {
	switch fx.Action {
	case "toggle":
		for _, tid := range fx.TargetIDs {
			if tid == "" {
				continue
			}
			target := findEntityInDispatch(dispatch, tid)
			currentState := "off"
			if target != nil {
				currentState = target.State
			}
			isOn := currentState != "on"
			states[tid] = isOn
			newState := "off"
			if isOn {
				newState = "on"
			}
			resp.Handled = true
			resp.Updates = append(resp.Updates, struct {
				EntityID string `json:"entity_id"`
				State    string `json:"state"`
			}{EntityID: tid, State: newState})
			if target != nil && target.GidOn != 0 {
				gid := target.GidOff
				if gid == 0 {
					gid = target.Gid
				}
				if isOn {
					gid = target.GidOn
				}
				resp.AppearanceUpdates = append(resp.AppearanceUpdates, struct {
					EntityID string `json:"entity_id"`
					Gid      uint32 `json:"gid"`
				}{EntityID: tid, Gid: gid})
			}
		}

	case "set_state":
		for _, tid := range fx.TargetIDs {
			if tid == "" {
				continue
			}
			newState := fx.Payload
			states[tid] = newState == "on"
			resp.Handled = true
			resp.Updates = append(resp.Updates, struct {
				EntityID string `json:"entity_id"`
				State    string `json:"state"`
			}{EntityID: tid, State: newState})
			target := findEntityInDispatch(dispatch, tid)
			if target != nil && target.GidOn != 0 {
				gid := target.GidOff
				if gid == 0 {
					gid = target.Gid
				}
				if newState == "on" {
					gid = target.GidOn
				}
				resp.AppearanceUpdates = append(resp.AppearanceUpdates, struct {
					EntityID string `json:"entity_id"`
					Gid      uint32 `json:"gid"`
				}{EntityID: tid, Gid: gid})
			}
		}

	case "activate", "turn_on":
		for _, tid := range fx.TargetIDs {
			if tid == "" {
				continue
			}
			states[tid] = true
			resp.Handled = true
			resp.Updates = append(resp.Updates, struct {
				EntityID string `json:"entity_id"`
				State    string `json:"state"`
			}{EntityID: tid, State: "on"})
			target := findEntityInDispatch(dispatch, tid)
			if target != nil && target.GidOn != 0 {
				resp.AppearanceUpdates = append(resp.AppearanceUpdates, struct {
					EntityID string `json:"entity_id"`
					Gid      uint32 `json:"gid"`
				}{EntityID: tid, Gid: target.GidOn})
			}
		}

	case "deactivate", "turn_off":
		for _, tid := range fx.TargetIDs {
			if tid == "" {
				continue
			}
			states[tid] = false
			resp.Handled = true
			resp.Updates = append(resp.Updates, struct {
				EntityID string `json:"entity_id"`
				State    string `json:"state"`
			}{EntityID: tid, State: "off"})
			target := findEntityInDispatch(dispatch, tid)
			if target != nil && target.GidOff != 0 {
				resp.AppearanceUpdates = append(resp.AppearanceUpdates, struct {
					EntityID string `json:"entity_id"`
					Gid      uint32 `json:"gid"`
				}{EntityID: tid, Gid: target.GidOff})
			} else if target != nil && target.Gid != 0 {
				resp.AppearanceUpdates = append(resp.AppearanceUpdates, struct {
					EntityID string `json:"entity_id"`
					Gid      uint32 `json:"gid"`
				}{EntityID: tid, Gid: target.Gid})
			}
		}

	// "toggle_wall", "send_notification", etc. -> not handled by ext-props
	}
}

// findEntityInDispatch searches both adjacent and target entities in the
// dispatch for the given entity ID. Returns nil if not found.
func findEntityInDispatch(dispatch *actionDispatchMsg, entityID string) *adjacentEntityInfo {
	for i := range dispatch.AdjacentEntities {
		if dispatch.AdjacentEntities[i].EntityID == entityID {
			return &dispatch.AdjacentEntities[i]
		}
	}
	for i := range dispatch.TargetEntities {
		if dispatch.TargetEntities[i].EntityID == entityID {
			return &dispatch.TargetEntities[i]
		}
	}
	return nil
}

// buildPopupAction returns the user-facing label for an action and
// whether it should be visible in the popup (optionally filtered by
// current state). The fallback shows the action string itself.
func buildPopupAction(actionID, currentState string) (label string, visible bool) {
	switch actionID {
	case "toggle":
		return "Toggle", true
	case "activate", "turn_on":
		return "Activate", currentState != "on"
	case "deactivate", "turn_off":
		return "Deactivate", currentState == "on"
	default:
		return strings.Title(actionID), true
	}
}

// splitAndTrim splits a comma-separated string and trims whitespace
// from each element, dropping empty strings.
func splitAndTrim(s string) []string {
	parts := strings.Split(s, ",")
	var result []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
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
