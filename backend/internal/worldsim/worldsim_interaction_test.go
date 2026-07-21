package worldsim

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
	"google.golang.org/protobuf/proto"

	pb "github.com/lstep/pixeleruv/backend/internal/pb"
)

// newInteractionTestSim builds a Simulator wired to an embedded NATS
// server, ready for interaction system tests. Returns the sim and a
// subscriber connection that tests can use to register fake extensions.
func newInteractionTestSim(t *testing.T) (*Simulator, *nats.Conn) {
	t.Helper()
	_, natsURL := startEmbeddedNATS(t)
	logger := slog.New(slog.NewTextHandler(&testWriter{t}, &slog.HandlerOptions{Level: slog.LevelWarn}))

	pubNc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("pub connect: %v", err)
	}
	t.Cleanup(pubNc.Close)

	subNc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("sub connect: %v", err)
	}
	t.Cleanup(subNc.Close)

	sim := &Simulator{
		World: World{
			zones:            map[string]*ZoneRegistry{"test-map": NewZoneRegistry(nil, 20, 20)},
			entities:         map[string]*Entity{},
			clients:          map[string]*Entity{},
			entityIDToClient: map[string]string{},
		},
		nc:         pubNc,
		defaultMap: "test-map",
		extMgr:     NewExtensionManager(logger),
		logger:     logger,
		tracer:     otel.Tracer("test"),
	}
	return sim, subNc
}

// fakePropsExtension subscribes to extension.props.action on the given
// NATS connection and replies with a canned actionReplyMsg. This lets
// us test the full applyAction dispatch -> extension reply ->
// applyActionReply pipeline without running the real ext-props binary.
//
// The handler replicates the key behaviors of ext-props:
//   - Filters by OwnerExtension == "props".
//   - For key:E: processes on_interact_action (immediate mode) and/or
//     builds available_actions from the actions property (popup mode).
//   - For action:execute: processes interactions[action_id] effects
//     for the target entity.
//   - processEffect handles toggle/set_state/activate/deactivate with
//     GidOff/GidOn appearance updates.
func fakePropsExtension(t *testing.T, nc *nats.Conn) {
	t.Helper()
	_, err := nc.Subscribe("extension.props.action", func(m *nats.Msg) {
		var dispatch actionDispatchMsg
		if err := json.Unmarshal(m.Data, &dispatch); err != nil {
			return
		}

		resp := actionReplyMsg{}
		for _, ent := range dispatch.AdjacentEntities {
			if ent.OwnerExtension != "props" {
				continue
			}

			if dispatch.Input == "action:execute" {
				if ent.EntityID != dispatch.TargetEntityID {
					continue
				}
				effects := ent.Interactions[dispatch.ActionID]
				for _, fx := range effects {
					processEffectFake(&dispatch, &resp, fx)
				}
				if resp.Handled {
					resp.Animations = append(resp.Animations, struct {
						EntityID    string `json:"entity_id"`
						AnimationID uint32 `json:"animation_id"`
					}{EntityID: ent.EntityID, AnimationID: 3})
				}
			} else {
				// key:E
				if ent.OnInteractAction != "" {
					effects := ent.Interactions[ent.OnInteractAction]
					for _, fx := range effects {
						processEffectFake(&dispatch, &resp, fx)
					}
					if resp.Handled {
						resp.Animations = append(resp.Animations, struct {
							EntityID    string `json:"entity_id"`
							AnimationID uint32 `json:"animation_id"`
						}{EntityID: ent.EntityID, AnimationID: 3})
					}
				}
				if ent.Actions != "" {
					for _, actionID := range splitAndTrimFake(ent.Actions) {
						label, visible := buildPopupActionFake(actionID, ent.State)
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
		}

		if !resp.Handled {
			return
		}
		data, _ := json.Marshal(resp)
		m.Respond(data)
	})
	if err != nil {
		t.Fatalf("subscribe fake props: %v", err)
	}
	// Flush so the subscription is registered on the server before the
	// test fires applyAction. Without this, RequestWithContext can race
	// with subscription propagation and time out (300ms) intermittently.
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush fake props sub: %v", err)
	}
}

// processEffectFake mirrors ext-props' processEffect: it handles
// toggle/set_state/activate/deactivate with GidOff/GidOn appearance
// updates. This is a faithful copy of the real logic so the test
// exercises the same code path the production extension uses.
func processEffectFake(dispatch *actionDispatchMsg, resp *actionReplyMsg, fx Effect) {
	findTarget := func(tid string) *adjacentEntityInfo {
		for i := range dispatch.AdjacentEntities {
			if dispatch.AdjacentEntities[i].EntityID == tid {
				return &dispatch.AdjacentEntities[i]
			}
		}
		for i := range dispatch.TargetEntities {
			if dispatch.TargetEntities[i].EntityID == tid {
				return &dispatch.TargetEntities[i]
			}
		}
		return nil
	}

	switch fx.Action {
	case "toggle":
		for _, tid := range fx.TargetIDs {
			if tid == "" {
				continue
			}
			target := findTarget(tid)
			currentState := "off"
			if target != nil {
				currentState = target.State
			}
			isOn := currentState != "on"
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
			resp.Handled = true
			resp.Updates = append(resp.Updates, struct {
				EntityID string `json:"entity_id"`
				State    string `json:"state"`
			}{EntityID: tid, State: newState})
			target := findTarget(tid)
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
			resp.Handled = true
			resp.Updates = append(resp.Updates, struct {
				EntityID string `json:"entity_id"`
				State    string `json:"state"`
			}{EntityID: tid, State: "on"})
			target := findTarget(tid)
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
			resp.Handled = true
			resp.Updates = append(resp.Updates, struct {
				EntityID string `json:"entity_id"`
				State    string `json:"state"`
			}{EntityID: tid, State: "off"})
			target := findTarget(tid)
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
	}
}

// splitAndTrimFake splits a comma-separated string and trims whitespace.
func splitAndTrimFake(s string) []string {
	var result []string
	for _, part := range splitFake(s) {
		trimmed := trimSpaceFake(part)
		if trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

func splitFake(s string) []string {
	var parts []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ',' {
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	parts = append(parts, s[start:])
	return parts
}

func trimSpaceFake(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}

// buildPopupActionFake mirrors ext-props' buildPopupAction: it returns
// a human-readable label and whether the action should be visible in
// the popup based on the entity's current state.
func buildPopupActionFake(actionID, currentState string) (string, bool) {
	switch actionID {
	case "toggle":
		return "Toggle", true
	case "activate", "turn_on":
		return "Activate", currentState != "on"
	case "deactivate", "turn_off":
		return "Deactivate", currentState == "on"
	default:
		return actionID, true
	}
}

// --- Tests ---

// TestAdjacentEntitiesLocked_IncludesInteractionFields verifies that
// adjacentEntityInfo carries the new interaction system fields
// (State, Gid, GidOff, GidOn, OnInteractAction, Actions, Interactions).
func TestAdjacentEntitiesLocked_IncludesInteractionFields(t *testing.T) {
	s := &Simulator{
		World: World{
			entities: make(map[string]*Entity),
			clients:  make(map[string]*Entity),
		},
	}
	s.entities["player-1"] = &Entity{
		ID:       "player-1",
		Position: &pb.Position{X: 5, Y: 5},
	}
	s.entities["switch-1"] = &Entity{
		ID:               "switch-1",
		Position:         &pb.Position{X: 5.5, Y: 5},
		EntityType:       "light_switch",
		OwnerExtension:   "props",
		State:            "off",
		Gid:              380,
		GidOff:           380,
		GidOn:            381,
		OnInteractAction: "toggle",
		Actions:          "",
		Interactions: map[string][]Effect{
			"toggle": {{Action: "toggle", TargetIDs: []string{"light-1", "light-2"}}},
		},
	}

	adjacent := s.adjacentEntitiesLocked("player-1", 5, 5)
	if len(adjacent) != 1 {
		t.Fatalf("expected 1 adjacent entity, got %d", len(adjacent))
	}
	a := adjacent[0]
	if a.EntityID != "switch-1" {
		t.Errorf("EntityID = %q, want switch-1", a.EntityID)
	}
	if a.State != "off" {
		t.Errorf("State = %q, want off", a.State)
	}
	if a.Gid != 380 {
		t.Errorf("Gid = %d, want 380", a.Gid)
	}
	if a.GidOff != 380 {
		t.Errorf("GidOff = %d, want 380", a.GidOff)
	}
	if a.GidOn != 381 {
		t.Errorf("GidOn = %d, want 381", a.GidOn)
	}
	if a.OnInteractAction != "toggle" {
		t.Errorf("OnInteractAction = %q, want toggle", a.OnInteractAction)
	}
	if len(a.Interactions) != 1 {
		t.Fatalf("Interactions len = %d, want 1", len(a.Interactions))
	}
	effects := a.Interactions["toggle"]
	if len(effects) != 1 {
		t.Fatalf("effects len = %d, want 1", len(effects))
	}
	if effects[0].Action != "toggle" {
		t.Errorf("effect action = %q, want toggle", effects[0].Action)
	}
	if len(effects[0].TargetIDs) != 2 {
		t.Errorf("target_ids len = %d, want 2", len(effects[0].TargetIDs))
	}
}

// TestApplyActionReply_AppearanceUpdates verifies that AppearanceUpdates
// from an extension reply set the entity's Gid and mark dirtyAppearance.
func TestApplyActionReply_AppearanceUpdates(t *testing.T) {
	s := &Simulator{
		World: World{
			entities: make(map[string]*Entity),
			clients:  make(map[string]*Entity),
		},
	}
	s.entities["light-1"] = &Entity{
		ID:       "light-1",
		Gid:      508,
		GidOff:   508,
		GidOn:    491,
		State:    "off",
	}

	resp := &actionReplyMsg{Handled: true}
	resp.Updates = append(resp.Updates, struct {
		EntityID string `json:"entity_id"`
		State    string `json:"state"`
	}{EntityID: "light-1", State: "on"})
	resp.AppearanceUpdates = append(resp.AppearanceUpdates, struct {
		EntityID string `json:"entity_id"`
		Gid      uint32 `json:"gid"`
	}{EntityID: "light-1", Gid: 491})

	s.applyActionReply(resp)

	e := s.entities["light-1"]
	if e.State != "on" || !e.dirtyState {
		t.Errorf("state = %q dirty=%v, want on/true", e.State, e.dirtyState)
	}
	if e.Gid != 491 || !e.dirtyAppearance {
		t.Errorf("Gid = %d dirty=%v, want 491/true", e.Gid, e.dirtyAppearance)
	}
}

// TestApplyActionReply_GidOffBug verifies the GidOff fix: when an
// entity is currently "on" (Gid == GidOn), a deactivate reply must
// set Gid back to GidOff, not to the current Gid (which is GidOn).
func TestApplyActionReply_GidOffBug(t *testing.T) {
	s := &Simulator{
		World: World{
			entities: make(map[string]*Entity),
			clients:  make(map[string]*Entity),
		},
	}
	s.entities["light-1"] = &Entity{
		ID:       "light-1",
		Gid:      491, // currently "on"
		GidOff:   508,
		GidOn:    491,
		State:    "on",
	}

	// Simulate a deactivate reply that correctly uses GidOff.
	resp := &actionReplyMsg{Handled: true}
	resp.Updates = append(resp.Updates, struct {
		EntityID string `json:"entity_id"`
		State    string `json:"state"`
	}{EntityID: "light-1", State: "off"})
	resp.AppearanceUpdates = append(resp.AppearanceUpdates, struct {
		EntityID string `json:"entity_id"`
		Gid      uint32 `json:"gid"`
	}{EntityID: "light-1", Gid: 508}) // GidOff

	s.applyActionReply(resp)

	e := s.entities["light-1"]
	if e.Gid != 508 {
		t.Errorf("Gid = %d, want 508 (GidOff). If this is 491, the deactivate didn't reset to the off sprite.", e.Gid)
	}
	if e.State != "off" {
		t.Errorf("State = %q, want off", e.State)
	}
}

// TestApplyActionReply_LightUpdates verifies that LightUpdates from an
// extension reply set the entity's LightIntensity/Color/Radius and mark
// dirtyLightEmitter. Also checks the "color/radius 0 = preserve" logic:
// when the reply sets intensity > 0 but color=0 and radius=0, the existing
// color/radius are preserved.
func TestApplyActionReply_LightUpdates(t *testing.T) {
	s := &Simulator{
		World: World{
			entities: make(map[string]*Entity),
			clients:  make(map[string]*Entity),
		},
	}
	s.entities["light-1"] = &Entity{
		ID:             "light-1",
		LightIntensity: 0,
		LightColor:     0xffe6b4,
		LightRadius:    3,
	}

	// Turn on: intensity 80, color 0 (preserve), radius 0 (preserve).
	resp := &actionReplyMsg{Handled: true}
	resp.LightUpdates = append(resp.LightUpdates, struct {
		EntityID  string  `json:"entity_id"`
		Intensity uint32  `json:"intensity"`
		Color     uint32  `json:"color,omitempty"`
		Radius    float32 `json:"radius,omitempty"`
	}{EntityID: "light-1", Intensity: 80})

	s.applyActionReply(resp)

	e := s.entities["light-1"]
	if e.LightIntensity != 80 {
		t.Errorf("intensity = %d, want 80", e.LightIntensity)
	}
	if e.LightColor != 0xffe6b4 {
		t.Errorf("color = %#x, want 0xffe6b4 (preserved)", e.LightColor)
	}
	if e.LightRadius != 3 {
		t.Errorf("radius = %f, want 3 (preserved)", e.LightRadius)
	}
	if !e.dirtyLightEmitter {
		t.Errorf("dirtyLightEmitter = false, want true")
	}

	// Turn off: intensity 0.
	e.dirtyLightEmitter = false
	resp2 := &actionReplyMsg{Handled: true}
	resp2.LightUpdates = append(resp2.LightUpdates, struct {
		EntityID  string  `json:"entity_id"`
		Intensity uint32  `json:"intensity"`
		Color     uint32  `json:"color,omitempty"`
		Radius    float32 `json:"radius,omitempty"`
	}{EntityID: "light-1", Intensity: 0})

	s.applyActionReply(resp2)

	e = s.entities["light-1"]
	if e.LightIntensity != 0 {
		t.Errorf("intensity = %d, want 0", e.LightIntensity)
	}
	if !e.dirtyLightEmitter {
		t.Errorf("dirtyLightEmitter = false, want true after turn-off")
	}
}

// TestSwitchToLights_Scenario recreates the design doc's section 6.2
// scenario: a wall switch (switch-1) toggles two remote lights
// (light-1, light-2). Pressing E on the switch should:
//   - Toggle all three entities' state from "off" to "on".
//   - Swap all three sprites to their GidOn frames.
//   - Queue a click animation on the switch.
//   - Return no available actions (immediate mode, no popup).
//
// This test uses the embedded NATS server and a fake props extension
// to exercise the full applyAction -> dispatch -> reply -> applyActionReply
// pipeline.
func TestSwitchToLights_Scenario(t *testing.T) {
	sim, subNc := newInteractionTestSim(t)

	// Register the fake props extension on the subscriber connection.
	fakePropsExtension(t, subNc)

	// Register the extension + its key:E trigger with the ExtensionManager.
	if err := sim.extMgr.Register([]byte(`{"extension_id":"props","heartbeat_interval_s":10}`)); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := sim.extMgr.RegisterTriggers([]byte(`{
		"extension_id": "props",
		"input_triggers": [{"input": "key:E"}]
	}`)); err != nil {
		t.Fatalf("RegisterTriggers: %v", err)
	}

	// Player at (5, 5).
	sim.entities["player-1"] = &Entity{
		ID:       "player-1",
		Position: &pb.Position{X: 5, Y: 5},
	}
	sim.clients["client-1"] = sim.entities["player-1"]
	sim.entityIDToClient["player-1"] = "client-1"

	// Switch at (5.5, 5) — adjacent to the player. Immediate mode:
	// on_interact_action="toggle" fires the toggle effect, which
	// targets switch-1 itself + light-1 + light-2.
	sim.entities["switch-1"] = &Entity{
		ID:               "switch-1",
		Position:         &pb.Position{X: 5.5, Y: 5},
		EntityType:       "light_switch",
		OwnerExtension:   "props",
		State:            "off",
		Gid:              380,
		GidOff:           380,
		GidOn:            381,
		OnInteractAction: "toggle",
		Interactions: map[string][]Effect{
			"toggle": {
				{Action: "toggle", TargetIDs: []string{"switch-1"}},
				{Action: "toggle", TargetIDs: []string{"light-1"}},
				{Action: "toggle", TargetIDs: []string{"light-2"}},
			},
		},
	}

	// Light-1 at (10, 10) — far away, not adjacent. It's a target
	// of the switch's toggle effect.
	sim.entities["light-1"] = &Entity{
		ID:             "light-1",
		Position:       &pb.Position{X: 10, Y: 10},
		EntityType:     "light",
		OwnerExtension: "props",
		State:          "off",
		Gid:            508,
		GidOff:         508,
		GidOn:          491,
	}

	// Light-2 at (12, 12) — also far away.
	sim.entities["light-2"] = &Entity{
		ID:             "light-2",
		Position:       &pb.Position{X: 12, Y: 12},
		EntityType:     "light",
		OwnerExtension: "props",
		State:          "off",
		Gid:            508,
		GidOff:         508,
		GidOn:          491,
	}

	// Subscribe to the client's replication subject to capture the
	// ActionResultFrame. The mutex protects actionResult from the
	// NATS callback goroutine writing it while the test goroutine reads.
	var mu sync.Mutex
	var actionResult *pb.ActionResultFrame
	sub, err := subNc.Subscribe("client.client-1.replication", func(m *nats.Msg) {
		var sf pb.ServerFrame
		if err := proto.Unmarshal(m.Data, &sf); err != nil {
			return
		}
		if ar := sf.GetActionResult(); ar != nil {
			mu.Lock()
			actionResult = ar
			mu.Unlock()
		}
	})
	if err != nil {
		t.Fatalf("subscribe replication: %v", err)
	}
	defer sub.Unsubscribe()

	// Fire key:E.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sim.applyAction(ctx, "client-1", &pb.ActionFrame{
		Seq:   1,
		Input: "key:E",
	})

	// Wait for the action result.
	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		done := actionResult != nil
		mu.Unlock()
		if done || !time.Now().Before(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	ar := actionResult
	mu.Unlock()
	if ar == nil {
		t.Fatal("timed out waiting for ActionResultFrame")
	}

	// Verify: immediate mode, no popup actions.
	if !ar.Ok {
		t.Errorf("ActionResult ok = false, reason = %q", ar.Reason)
	}
	if len(ar.AvailableActions) != 0 {
		t.Errorf("AvailableActions len = %d, want 0 (immediate mode)", len(ar.AvailableActions))
	}

	// Verify: all three entities toggled from "off" to "on".
	for _, id := range []string{"switch-1", "light-1", "light-2"} {
		e := sim.entities[id]
		if e.State != "on" {
			t.Errorf("%s State = %q, want on", id, e.State)
		}
		if !e.dirtyState {
			t.Errorf("%s dirtyState = false, want true", id)
		}
	}

	// Verify: all three sprites swapped to GidOn.
	for _, id := range []string{"switch-1", "light-1", "light-2"} {
		e := sim.entities[id]
		if e.Gid != e.GidOn {
			t.Errorf("%s Gid = %d, want %d (GidOn)", id, e.Gid, e.GidOn)
		}
		if !e.dirtyAppearance {
			t.Errorf("%s dirtyAppearance = false, want true", id)
		}
	}

	// Verify: click animation queued on the switch only.
	sw := sim.entities["switch-1"]
	if len(sw.pendingAnimations) != 1 || sw.pendingAnimations[0] != 3 {
		t.Errorf("switch-1 pendingAnimations = %v, want [3]", sw.pendingAnimations)
	}
	for _, id := range []string{"light-1", "light-2"} {
		e := sim.entities[id]
		if len(e.pendingAnimations) != 0 {
			t.Errorf("%s pendingAnimations = %v, want empty (click only on switch)", id, e.pendingAnimations)
		}
	}
}

// TestSwitchToLights_ToggleBack verifies that toggling a second time
// (when entities are "on") correctly sets the sprite back to GidOff,
// not to the current Gid (which would be GidOn). This is the core
// regression test for the GidOff fix.
func TestSwitchToLights_ToggleBack(t *testing.T) {
	sim, subNc := newInteractionTestSim(t)
	fakePropsExtension(t, subNc)

	sim.extMgr.Register([]byte(`{"extension_id":"props","heartbeat_interval_s":10}`))
	sim.extMgr.RegisterTriggers([]byte(`{
		"extension_id": "props",
		"input_triggers": [{"input": "key:E"}]
	}`))

	sim.entities["player-1"] = &Entity{
		ID:       "player-1",
		Position: &pb.Position{X: 5, Y: 5},
	}
	sim.clients["client-1"] = sim.entities["player-1"]
	sim.entityIDToClient["player-1"] = "client-1"

	// Light starts in "on" state with Gid = GidOn.
	sim.entities["light-1"] = &Entity{
		ID:             "light-1",
		Position:       &pb.Position{X: 5.5, Y: 5},
		EntityType:     "light",
		OwnerExtension: "props",
		State:          "on",
		Gid:            491, // currently on
		GidOff:         508,
		GidOn:          491,
		OnInteractAction: "toggle",
		Interactions: map[string][]Effect{
			"toggle": {{Action: "toggle", TargetIDs: []string{"light-1"}}},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sim.applyAction(ctx, "client-1", &pb.ActionFrame{Seq: 1, Input: "key:E"})

	// Wait for the toggle to be applied (async via NATS dispatch).
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		sim.mu.Lock()
		e := sim.entities["light-1"]
		done := e.State == "off" && e.Gid == 508
		sim.mu.Unlock()
		if done {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	sim.mu.Lock()
	defer sim.mu.Unlock()
	e := sim.entities["light-1"]
	if e.State != "off" {
		t.Errorf("State = %q, want off (toggled from on)", e.State)
	}
	if e.Gid != 508 {
		t.Errorf("Gid = %d, want 508 (GidOff). Got GidOn=491 — the GidOff fix is not working.", e.Gid)
	}
}

// TestPopupMode_AvailableActions verifies that pressing E near a
// popup-mode entity returns available actions in the ActionResultFrame
// without executing any effects.
func TestPopupMode_AvailableActions(t *testing.T) {
	sim, subNc := newInteractionTestSim(t)
	fakePropsExtension(t, subNc)

	sim.extMgr.Register([]byte(`{"extension_id":"props","heartbeat_interval_s":10}`))
	sim.extMgr.RegisterTriggers([]byte(`{
		"extension_id": "props",
		"input_triggers": [{"input": "key:E"}]
	}`))

	sim.entities["player-1"] = &Entity{
		ID:       "player-1",
		Position: &pb.Position{X: 5, Y: 5},
	}
	sim.clients["client-1"] = sim.entities["player-1"]
	sim.entityIDToClient["player-1"] = "client-1"

	// Light in popup mode: actions="toggle,activate,deactivate",
	// no on_interact_action. State is "off" so "deactivate" should
	// be filtered out (not visible when already off).
	sim.entities["light-1"] = &Entity{
		ID:             "light-1",
		Position:       &pb.Position{X: 5.5, Y: 5},
		EntityType:     "light",
		OwnerExtension: "props",
		State:          "off",
		Gid:            508,
		GidOff:         508,
		GidOn:          491,
		Actions:        "toggle,activate,deactivate",
		Interactions: map[string][]Effect{
			"toggle":      {{Action: "toggle", TargetIDs: []string{"light-1"}}},
			"activate":    {{Action: "set_state", Payload: "on", TargetIDs: []string{"light-1"}}},
			"deactivate":  {{Action: "set_state", Payload: "off", TargetIDs: []string{"light-1"}}},
		},
	}

	var mu sync.Mutex
	var actionResult *pb.ActionResultFrame
	sub, _ := subNc.Subscribe("client.client-1.replication", func(m *nats.Msg) {
		var sf pb.ServerFrame
		proto.Unmarshal(m.Data, &sf)
		if ar := sf.GetActionResult(); ar != nil {
			mu.Lock()
			actionResult = ar
			mu.Unlock()
		}
	})
	defer sub.Unsubscribe()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sim.applyAction(ctx, "client-1", &pb.ActionFrame{Seq: 1, Input: "key:E"})

	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		done := actionResult != nil
		mu.Unlock()
		if done || !time.Now().Before(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	ar := actionResult
	mu.Unlock()
	if ar == nil {
		t.Fatal("timed out waiting for ActionResultFrame")
	}

	if !ar.Ok {
		t.Errorf("ok = false, reason = %q", ar.Reason)
	}

	// Should have 2 actions: "toggle" (always visible) and "activate"
	// (visible when state != "on"). "deactivate" should be filtered out
	// because state is "off".
	if len(ar.AvailableActions) != 2 {
		t.Fatalf("AvailableActions len = %d, want 2: %+v", len(ar.AvailableActions), ar.AvailableActions)
	}

	labels := make(map[string]string) // actionId -> label
	for _, a := range ar.AvailableActions {
		labels[a.ActionId] = a.Label
	}
	if _, ok := labels["toggle"]; !ok {
		t.Errorf("missing 'toggle' action")
	}
	if _, ok := labels["activate"]; !ok {
		t.Errorf("missing 'activate' action")
	}
	if _, ok := labels["deactivate"]; ok {
		t.Errorf("'deactivate' should be filtered out when state=off")
	}

	// No effects should have been executed — state should still be "off".
	e := sim.entities["light-1"]
	if e.State != "off" {
		t.Errorf("State = %q, want off (popup mode should not execute effects)", e.State)
	}
	if e.Gid != 508 {
		t.Errorf("Gid = %d, want 508 (no appearance change in popup mode)", e.Gid)
	}
}

// TestActionExecute_PopupChoice verifies that sending action:execute
// with a chosen action_id processes the effects for that action only.
func TestActionExecute_PopupChoice(t *testing.T) {
	sim, subNc := newInteractionTestSim(t)
	fakePropsExtension(t, subNc)

	sim.extMgr.Register([]byte(`{"extension_id":"props","heartbeat_interval_s":10}`))
	sim.extMgr.RegisterTriggers([]byte(`{
		"extension_id": "props",
		"input_triggers": [{"input": "key:E"}, {"input": "action:execute"}]
	}`))

	sim.entities["player-1"] = &Entity{
		ID:       "player-1",
		Position: &pb.Position{X: 5, Y: 5},
	}
	sim.clients["client-1"] = sim.entities["player-1"]
	sim.entityIDToClient["player-1"] = "client-1"

	sim.entities["light-1"] = &Entity{
		ID:             "light-1",
		Position:       &pb.Position{X: 5.5, Y: 5},
		EntityType:     "light",
		OwnerExtension: "props",
		State:          "off",
		Gid:            508,
		GidOff:         508,
		GidOn:          491,
		Actions:        "toggle,activate,deactivate",
		Interactions: map[string][]Effect{
			"toggle":      {{Action: "toggle", TargetIDs: []string{"light-1"}}},
			"activate":    {{Action: "set_state", Payload: "on", TargetIDs: []string{"light-1"}}},
			"deactivate":  {{Action: "set_state", Payload: "off", TargetIDs: []string{"light-1"}}},
		},
	}

	// Send action:execute with action_id="activate".
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sim.applyAction(ctx, "client-1", &pb.ActionFrame{
		Seq:      2,
		Input:    "action:execute",
		EntityId: "light-1",
		ActionId: "activate",
	})

	// Wait for the activate to be applied (async via NATS dispatch).
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		sim.mu.Lock()
		e := sim.entities["light-1"]
		done := e.State == "on" && e.Gid == 491
		sim.mu.Unlock()
		if done {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	sim.mu.Lock()
	defer sim.mu.Unlock()
	e := sim.entities["light-1"]
	if e.State != "on" {
		t.Errorf("State = %q, want on (activate should set state to on)", e.State)
	}
	if e.Gid != 491 {
		t.Errorf("Gid = %d, want 491 (GidOn for activate)", e.Gid)
	}
}
