package worldsim

import "testing"

func TestParseTiledMapJSON_Entities(t *testing.T) {
	body := []byte(`{
		"width": 10,
		"height": 10,
		"tilewidth": 32,
		"tileheight": 32,
		"layers": [
			{
				"name": "Entities",
				"type": "objectgroup",
				"objects": [
					{
						"name": "light-switch-box-1",
						"x": 160,
						"y": 96,
						"width": 32,
						"height": 32,
						"properties": [
							{"name": "entity_type", "type": "string", "value": "light_switch"},
							{"name": "owner_extension", "type": "string", "value": "ext-props"},
							{"name": "trigger_radius", "type": "float", "value": 1.5}
						]
					},
					{"name": "", "x": 0, "y": 0}
				]
			}
		]
	}`)

	md, err := parseTiledMapJSON(body)
	if err != nil {
		t.Fatalf("parseTiledMapJSON: %v", err)
	}
	if len(md.Entities) != 1 {
		t.Fatalf("expected 1 entity (unnamed object skipped), got %d", len(md.Entities))
	}
	e := md.Entities[0]
	if e.ID != "light-switch-box-1" {
		t.Errorf("ID = %q, want light-switch-box-1", e.ID)
	}
	if e.X != 5 || e.Y != 3 {
		t.Errorf("X,Y = %v,%v, want 5,3 (160/32, 96/32)", e.X, e.Y)
	}
	if e.EntityType != "light_switch" {
		t.Errorf("EntityType = %q, want light_switch", e.EntityType)
	}
	if e.OwnerExtension != "ext-props" {
		t.Errorf("OwnerExtension = %q, want ext-props", e.OwnerExtension)
	}
	if e.TriggerRadius != 1.5 {
		t.Errorf("TriggerRadius = %v, want 1.5", e.TriggerRadius)
	}
}

func TestParseTiledMapJSON_NoEntitiesLayer(t *testing.T) {
	body := []byte(`{"width": 5, "height": 5, "layers": []}`)
	md, err := parseTiledMapJSON(body)
	if err != nil {
		t.Fatalf("parseTiledMapJSON: %v", err)
	}
	if len(md.Entities) != 0 {
		t.Errorf("expected no entities, got %d", len(md.Entities))
	}
}

// TestParseTiledMapJSON_InteractionProperties verifies that the new
// interaction system Tiled properties (gid_on, on_interact_action,
// actions, interactions) are parsed correctly into PropEntity fields.
func TestParseTiledMapJSON_InteractionProperties(t *testing.T) {
	body := []byte(`{
		"width": 10,
		"height": 10,
		"tilewidth": 32,
		"tileheight": 32,
		"layers": [
			{
				"name": "Entities",
				"type": "objectgroup",
				"objects": [
					{
						"name": "switch-1",
						"x": 160,
						"y": 96,
						"width": 32,
						"height": 32,
						"properties": [
							{"name": "entity_type", "type": "string", "value": "light_switch"},
							{"name": "owner_extension", "type": "string", "value": "props"},
							{"name": "trigger_radius", "type": "float", "value": 1.5},
							{"name": "gid_on", "type": "int", "value": 381},
							{"name": "on_interact_action", "type": "string", "value": "toggle"},
							{"name": "interactions", "type": "string", "value": "{\"toggle\":[{\"action\":\"toggle\",\"target_ids\":[\"light-1\",\"light-2\"]}]}"}
						]
					},
					{
						"name": "light-1",
						"x": 320,
						"y": 320,
						"width": 32,
						"height": 32,
						"properties": [
							{"name": "entity_type", "type": "string", "value": "light"},
							{"name": "owner_extension", "type": "string", "value": "props"},
							{"name": "gid_on", "type": "int", "value": 491},
							{"name": "actions", "type": "string", "value": "toggle,activate,deactivate"},
							{"name": "interactions", "type": "string", "value": "{\"toggle\":[{\"action\":\"toggle\",\"target_ids\":[\"light-1\"]}],\"activate\":[{\"action\":\"set_state\",\"payload\":\"on\",\"target_ids\":[\"light-1\"]}],\"deactivate\":[{\"action\":\"set_state\",\"payload\":\"off\",\"target_ids\":[\"light-1\"]}]}"}
						]
					}
				]
			}
		]
	}`)

	md, err := parseTiledMapJSON(body)
	if err != nil {
		t.Fatalf("parseTiledMapJSON: %v", err)
	}
	if len(md.Entities) != 2 {
		t.Fatalf("expected 2 entities, got %d", len(md.Entities))
	}

	// switch-1: immediate mode
	sw := md.Entities[0]
	if sw.ID != "switch-1" {
		t.Errorf("switch ID = %q, want switch-1", sw.ID)
	}
	if sw.GidOn != 381 {
		t.Errorf("switch GidOn = %d, want 381", sw.GidOn)
	}
	if sw.OnInteractAction != "toggle" {
		t.Errorf("switch OnInteractAction = %q, want toggle", sw.OnInteractAction)
	}
	if sw.Actions != "" {
		t.Errorf("switch Actions = %q, want empty (immediate mode)", sw.Actions)
	}
	if len(sw.Interactions) != 1 {
		t.Fatalf("switch Interactions len = %d, want 1", len(sw.Interactions))
	}
	effects := sw.Interactions["toggle"]
	if len(effects) != 1 {
		t.Fatalf("switch toggle effects len = %d, want 1", len(effects))
	}
	if effects[0].Action != "toggle" {
		t.Errorf("switch effect action = %q, want toggle", effects[0].Action)
	}
	if len(effects[0].TargetIDs) != 2 {
		t.Errorf("switch target_ids len = %d, want 2", len(effects[0].TargetIDs))
	}
	if effects[0].TargetIDs[0] != "light-1" || effects[0].TargetIDs[1] != "light-2" {
		t.Errorf("switch target_ids = %v, want [light-1 light-2]", effects[0].TargetIDs)
	}

	// light-1: popup mode
	light := md.Entities[1]
	if light.ID != "light-1" {
		t.Errorf("light ID = %q, want light-1", light.ID)
	}
	if light.GidOn != 491 {
		t.Errorf("light GidOn = %d, want 491", light.GidOn)
	}
	if light.OnInteractAction != "" {
		t.Errorf("light OnInteractAction = %q, want empty (popup mode)", light.OnInteractAction)
	}
	if light.Actions != "toggle,activate,deactivate" {
		t.Errorf("light Actions = %q, want toggle,activate,deactivate", light.Actions)
	}
	if len(light.Interactions) != 3 {
		t.Fatalf("light Interactions len = %d, want 3", len(light.Interactions))
	}
	// Verify the activate effect has a payload.
	activateEffects := light.Interactions["activate"]
	if len(activateEffects) != 1 {
		t.Fatalf("activate effects len = %d, want 1", len(activateEffects))
	}
	if activateEffects[0].Action != "set_state" {
		t.Errorf("activate action = %q, want set_state", activateEffects[0].Action)
	}
	if activateEffects[0].Payload != "on" {
		t.Errorf("activate payload = %q, want on", activateEffects[0].Payload)
	}
}

// TestParseTiledMapJSON_LightAttributes verifies that the light_intensity,
// light_color, and light_radius Tiled properties are parsed into PropEntity
// fields. light_color accepts "#RRGGBB" and "RRGGBB" forms; malformed values
// parse to 0 (caller treats as default).
func TestParseTiledMapJSON_LightAttributes(t *testing.T) {
	body := []byte(`{
		"width": 10,
		"height": 10,
		"tilewidth": 32,
		"tileheight": 32,
		"layers": [
			{
				"name": "Entities",
				"type": "objectgroup",
				"objects": [
					{
						"name": "light-a",
						"x": 64,
						"y": 64,
						"width": 32,
						"height": 32,
						"properties": [
							{"name": "light_intensity", "type": "int", "value": 80},
							{"name": "light_color", "type": "string", "value": "#ffe6b4"},
							{"name": "light_radius", "type": "float", "value": 4.5}
						]
					},
					{
						"name": "light-b",
						"x": 128,
						"y": 128,
						"width": 32,
						"height": 32,
						"properties": [
							{"name": "light_intensity", "type": "int", "value": 50},
							{"name": "light_color", "type": "string", "value": "ff0000"}
						]
					},
					{
						"name": "light-c",
						"x": 192,
						"y": 192,
						"width": 32,
						"height": 32,
						"properties": [
							{"name": "light_color", "type": "string", "value": "not-a-color"}
						]
					}
				]
			}
		]
	}`)

	md, err := parseTiledMapJSON(body)
	if err != nil {
		t.Fatalf("parseTiledMapJSON: %v", err)
	}
	if len(md.Entities) != 3 {
		t.Fatalf("expected 3 entities, got %d", len(md.Entities))
	}

	a := md.Entities[0]
	if a.LightIntensity != 80 {
		t.Errorf("light-a intensity = %d, want 80", a.LightIntensity)
	}
	if a.LightColor != 0xffe6b4 {
		t.Errorf("light-a color = %#x, want 0xffe6b4", a.LightColor)
	}
	if a.LightRadius != 4.5 {
		t.Errorf("light-a radius = %f, want 4.5", a.LightRadius)
	}

	b := md.Entities[1]
	if b.LightIntensity != 50 {
		t.Errorf("light-b intensity = %d, want 50", b.LightIntensity)
	}
	if b.LightColor != 0xff0000 {
		t.Errorf("light-b color = %#x, want 0xff0000", b.LightColor)
	}
	if b.LightRadius != 0 {
		t.Errorf("light-b radius = %f, want 0 (unset)", b.LightRadius)
	}

	c := md.Entities[2]
	if c.LightIntensity != 0 {
		t.Errorf("light-c intensity = %d, want 0 (unset)", c.LightIntensity)
	}
	if c.LightColor != 0 {
		t.Errorf("light-c color = %#x, want 0 (malformed)", c.LightColor)
	}
}

// TestParseHexColor tests the parseHexColor helper directly.
func TestParseHexColor(t *testing.T) {
	cases := []struct {
		in   string
		want uint32
	}{
		{"#ffe6b4", 0xffe6b4},
		{"ffe6b4", 0xffe6b4},
		{"#FF0000", 0xff0000},
		{"000000", 0x000000},
		{"#fff", 0},        // too short
		{"not-a-color", 0}, // non-hex
		{"", 0},            // empty
		{"#gggggg", 0},     // non-hex chars
	}
	for _, c := range cases {
		got := parseHexColor(c.in)
		if got != c.want {
			t.Errorf("parseHexColor(%q) = %#x, want %#x", c.in, got, c.want)
		}
	}
}
