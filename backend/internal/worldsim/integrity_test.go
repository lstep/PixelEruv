package worldsim

import (
	"testing"
)

// hasIssueMatching returns true if any result in results has the given level
// and a message containing substr.
func hasIssueMatching(results []CheckResult, level CheckLevel, substr string) bool {
	for _, r := range results {
		if r.Level == level && contains(r.Message, substr) {
			return true
		}
	}
	return false
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && stringContains(s, sub))
}

// stringContains is a thin wrapper to avoid importing strings in a test
// helper (keeps the file self-contained).
func stringContains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// entityMap builds a minimal valid MapData with the given entities and a
// 10x10 collision grid (all walkable) so entity/trigger checks run.
func entityMap(entities ...PropEntity) *MapData {
	collision := make([][]bool, 10)
	for y := range collision {
		collision[y] = make([]bool, 10)
	}
	ptrs := make([]*PropEntity, len(entities))
	for i := range entities {
		ptrs[i] = &entities[i]
	}
	return &MapData{
		Width:     10,
		Height:    10,
		Collision: collision,
		Entities:  ptrs,
	}
}

func TestCheckMapIntegrity_DuplicateEntityID_IsFatal(t *testing.T) {
	md := entityMap(
		PropEntity{ID: "light-1", Interactions: map[string][]Effect{}},
		PropEntity{ID: "light-1", Interactions: map[string][]Effect{}},
	)
	results := CheckMapIntegrity(md)
	if !hasIssueMatching(results, LevelError, "duplicate entity ID") {
		t.Errorf("expected fatal duplicate-entity-ID error, got: %v", results)
	}
}

func TestCheckMapIntegrity_EmptyAction_IsFatal(t *testing.T) {
	md := entityMap(PropEntity{
		ID: "switch-1",
		Interactions: map[string][]Effect{
			"toggle": {{Action: "", TargetIDs: []string{"switch-1"}}},
		},
	})
	results := CheckMapIntegrity(md)
	if !hasIssueMatching(results, LevelError, "empty action") {
		t.Errorf("expected fatal empty-action error, got: %v", results)
	}
}

func TestCheckMapIntegrity_NoTargetIDs_IsFatal(t *testing.T) {
	md := entityMap(PropEntity{
		ID: "switch-1",
		Interactions: map[string][]Effect{
			"toggle": {{Action: "toggle"}}, // no TargetIDs
		},
	})
	results := CheckMapIntegrity(md)
	if !hasIssueMatching(results, LevelError, "no target_ids") {
		t.Errorf("expected fatal no-target_ids error, got: %v", results)
	}
}

func TestCheckMapIntegrity_UnknownActionVerb_Warns(t *testing.T) {
	md := entityMap(PropEntity{
		ID: "switch-1",
		Interactions: map[string][]Effect{
			"frob": {{Action: "frob", TargetIDs: []string{"switch-1"}}},
		},
	})
	results := CheckMapIntegrity(md)
	if !hasIssueMatching(results, LevelWarning, "unknown action verb") {
		t.Errorf("expected unknown-action-verb warning, got: %v", results)
	}
	// Unknown verb is a warning, not fatal.
	if hasIssueMatching(results, LevelError, "unknown action verb") {
		t.Errorf("unknown action verb should be a warning, not an error")
	}
}

func TestCheckMapIntegrity_DanglingTargetID_Warns(t *testing.T) {
	md := entityMap(PropEntity{
		ID: "switch-1",
		Interactions: map[string][]Effect{
			"toggle": {{Action: "toggle", TargetIDs: []string{"ghost-light"}}},
		},
	})
	results := CheckMapIntegrity(md)
	if !hasIssueMatching(results, LevelWarning, "does not exist on this map") {
		t.Errorf("expected dangling-target warning, got: %v", results)
	}
}

func TestCheckMapIntegrity_LightIntensityOver100_Warns(t *testing.T) {
	md := entityMap(PropEntity{
		ID:             "light-1",
		LightIntensity: 150,
		Interactions:   map[string][]Effect{},
	})
	results := CheckMapIntegrity(md)
	if !hasIssueMatching(results, LevelWarning, "light_intensity 150 > 100") {
		t.Errorf("expected light_intensity warning, got: %v", results)
	}
}

func TestCheckMapIntegrity_NegativeLightRadius_Warns(t *testing.T) {
	md := entityMap(PropEntity{
		ID:          "light-1",
		LightRadius: -2,
		Interactions: map[string][]Effect{},
	})
	results := CheckMapIntegrity(md)
	if !hasIssueMatching(results, LevelWarning, "light_radius") {
		t.Errorf("expected light_radius warning, got: %v", results)
	}
}

func TestCheckMapIntegrity_ValidInteractions_NoIssues(t *testing.T) {
	md := entityMap(
		PropEntity{
			ID:             "light-1",
			LightIntensity: 50,
			LightRadius:    3,
			Interactions:   map[string][]Effect{},
		},
		PropEntity{
			ID: "switch-1",
			Interactions: map[string][]Effect{
				"toggle": {{Action: "toggle", TargetIDs: []string{"light-1"}}},
			},
		},
	)
	results := CheckMapIntegrity(md)
	for _, r := range results {
		if r.Level == LevelError || r.Level == LevelWarning {
			t.Errorf("unexpected issue for valid map: %v", r)
		}
	}
}

func TestCheckMapIntegrity_WarningCarriesEntityID(t *testing.T) {
	md := entityMap(PropEntity{
		ID: "switch-1",
		Interactions: map[string][]Effect{
			"toggle": {{Action: "toggle", TargetIDs: []string{"ghost"}}},
		},
	})
	results := CheckMapIntegrity(md)
	found := false
	for _, r := range results {
		if r.Level == LevelWarning && r.EntityID == "switch-1" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a warning with EntityID=switch-1, got: %v", results)
	}
}
