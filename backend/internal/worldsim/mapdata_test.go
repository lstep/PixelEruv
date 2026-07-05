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
