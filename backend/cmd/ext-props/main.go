// ext-props is a first-party extension that handles entity state and
// appearance interactions (see
// documentation/plans/2026-07-15-interaction-system-design.md). It
// registers for "key:E" and "action:execute" inputs, reads the
// entity's interactions data from the dispatch payload, and processes
// the effects it knows how to handle (toggle, set_state, activate,
// deactivate, turn_on, turn_off, set_light, toggle_light). Effects it
// doesn't know (toggle_wall, send_notification, etc.) are silently
// skipped — those are handled by other extensions.
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
	"github.com/lstep/pixeleruv/backend/internal/extkit"
	"github.com/lstep/pixeleruv/backend/internal/otel"
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
	// GidOn/GidOff are optional per-effect sprite overrides for verbs that
	// swap the target's sprite (toggle, set_state, toggle_light, set_light).
	// 0 means fall back to the target entity's own GidOn/GidOff.
	GidOn  uint32 `json:"gid_on,omitempty"`
	GidOff uint32 `json:"gid_off,omitempty"`
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
	LightIntensity   uint32              `json:"light_intensity,omitempty"`
	LightColor       uint32              `json:"light_color,omitempty"`
	LightRadius      float32             `json:"light_radius,omitempty"`
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
	LightUpdates []struct {
		EntityID  string  `json:"entity_id"`
		Intensity uint32  `json:"intensity"`
		Color     uint32  `json:"color,omitempty"`
		Radius    float32 `json:"radius,omitempty"`
	} `json:"light_updates,omitempty"`
	Animations []struct {
		EntityID    string `json:"entity_id"`
		AnimationID uint32 `json:"animation_id"`
	} `json:"animations,omitempty"`
	AvailableActions []availableAction `json:"available_actions,omitempty"`
}

func main() {
	natsURL := extkit.EnvOr("NATS_URL", "nats://localhost:4222")
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

	nc, err := extkit.ConnectNATS(natsURL, extID)
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
	if err := extkit.SubscribeOptions(nc, extID, &opts, &mu, logger, func() {
		logger.Info("options updated", "interaction_radius", opts.InteractionRadius)
	}); err != nil {
		logger.Error("subscribe options", "err", err)
		os.Exit(1)
	}

	// extension.props.action — dispatched for "key:E" and "action:execute".
	// The handler reads the entity's interactions data and processes the
	// effects it knows (toggle, set_state, activate, deactivate, turn_on,
	// turn_off, set_light, toggle_light). Unknown action verbs are silently
	// skipped.
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

	regData, _ := json.Marshal(extkit.RegisterMsg{
		ExtensionID:        extID,
		HeartbeatIntervalS: heartbeatS,
		OptionsSchema: []extkit.OptionFieldDef{
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
	extkit.WaitForReady(nc, logger, 10*time.Second, func(_ string) {
		publishReg()
	})
	logger.Info("registered props extension", "inputs", []string{inputKeyE, inputExecute})

	// Heartbeat + re-register loop. onReRegister publishes reg+trig only
	// (HeartbeatLoop handles the heartbeat + health publish).
	extkit.HeartbeatLoop(ctx, nc, extID, heartbeatS, func() {
		nc.Publish(regSubject, regData)
		nc.Publish(trigSubject, trigData)
	})
	logger.Info("shutting down")
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
			if gid, ok := resolveGidSwap(fx, target, isOn); ok {
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
			if gid, ok := resolveGidSwap(fx, target, newState == "on"); ok {
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

	case "set_light":
		// Payload is the target intensity as a string number ("0"-"100").
		// color/radius are preserved from the entity's current values
		// (worldsim treats 0 as "unchanged" when intensity is non-zero).
		// Also swaps the target's sprite gid: intensity > 0 -> GidOn,
		// intensity == 0 -> GidOff (or Gid if GidOff is 0), so a single
		// set_light effect handles both the light and the sprite frame.
		// fx.GidOn/GidOff override the target's own values when non-zero.
		for _, tid := range fx.TargetIDs {
			if tid == "" {
				continue
			}
			var intensity uint32
			_, _ = fmt.Sscanf(fx.Payload, "%d", &intensity)
			if intensity > 100 {
				intensity = 100
			}
			resp.Handled = true
			resp.LightUpdates = append(resp.LightUpdates, struct {
				EntityID  string  `json:"entity_id"`
				Intensity uint32  `json:"intensity"`
				Color     uint32  `json:"color,omitempty"`
				Radius    float32 `json:"radius,omitempty"`
			}{EntityID: tid, Intensity: intensity})
			target := findEntityInDispatch(dispatch, tid)
			if gid, ok := resolveGidSwap(fx, target, intensity > 0); ok {
				resp.AppearanceUpdates = append(resp.AppearanceUpdates, struct {
					EntityID string `json:"entity_id"`
					Gid      uint32 `json:"gid"`
				}{EntityID: tid, Gid: gid})
			}
		}

	case "toggle_light":
		// Flips between 0 and a stored "on" intensity. If the target's
		// current intensity is > 0, turn off (0); otherwise turn on to
		// a default of 80 (or the value in Payload if provided). Also
		// swaps the target's sprite gid to match (GidOn when lit,
		// GidOff/Gid when unlit), so a single toggle_light effect
		// handles both the light and the sprite frame. fx.GidOn/GidOff
		// override the target's own values when non-zero.
		for _, tid := range fx.TargetIDs {
			if tid == "" {
				continue
			}
			target := findEntityInDispatch(dispatch, tid)
			currentIntensity := uint32(0)
			if target != nil {
				currentIntensity = target.LightIntensity
			}
			var newIntensity uint32
			if currentIntensity > 0 {
				newIntensity = 0
			} else {
				newIntensity = 80
				if fx.Payload != "" {
					_, _ = fmt.Sscanf(fx.Payload, "%d", &newIntensity)
					if newIntensity > 100 {
						newIntensity = 100
					}
				}
			}
			resp.Handled = true
			resp.LightUpdates = append(resp.LightUpdates, struct {
				EntityID  string  `json:"entity_id"`
				Intensity uint32  `json:"intensity"`
				Color     uint32  `json:"color,omitempty"`
				Radius    float32 `json:"radius,omitempty"`
			}{EntityID: tid, Intensity: newIntensity})
			if gid, ok := resolveGidSwap(fx, target, newIntensity > 0); ok {
				resp.AppearanceUpdates = append(resp.AppearanceUpdates, struct {
					EntityID string `json:"entity_id"`
					Gid      uint32 `json:"gid"`
				}{EntityID: tid, Gid: gid})
			}
		}

	// "toggle_wall", "send_notification", etc. -> not handled by ext-props
	}
}

// resolveGidSwap picks the sprite gid for a target given the effect's
// per-effect GidOn/GidOff overrides (non-zero wins) and the target's own
// GidOn/GidOff/Gid as fallback. lit=true selects the "on" frame, lit=false
// the "off" frame. Returns (gid, false) when no gid swap should be emitted
// (neither the effect nor the target defines a GidOn).
func resolveGidSwap(fx Effect, target *adjacentEntityInfo, lit bool) (uint32, bool) {
	gidOn := fx.GidOn
	gidOff := fx.GidOff
	if target != nil {
		if gidOn == 0 {
			gidOn = target.GidOn
		}
		if gidOff == 0 {
			gidOff = target.GidOff
		}
	}
	if gidOn == 0 {
		return 0, false
	}
	gid := gidOff
	if gid == 0 && target != nil {
		gid = target.Gid
	}
	if lit {
		gid = gidOn
	}
	return gid, true
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
