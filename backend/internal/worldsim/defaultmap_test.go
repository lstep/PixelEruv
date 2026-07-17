package worldsim

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSelectDefaultMap(t *testing.T) {
	mk := func(name string, isDefault bool) *MapRecordInfo {
		return &MapRecordInfo{Name: name, IsDefault: isDefault}
	}

	t.Run("returns the is_default map", func(t *testing.T) {
		records := []*MapRecordInfo{
			mk("main", true),
			mk("office", false),
		}
		got, err := selectDefaultMap(records)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "main" {
			t.Fatalf("got %q, want %q", got, "main")
		}
	})

	t.Run("returns first is_default when multiple are flagged", func(t *testing.T) {
		records := []*MapRecordInfo{
			mk("a", false),
			mk("b", true),
			mk("c", true),
		}
		got, err := selectDefaultMap(records)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "b" {
			t.Fatalf("got %q, want %q", got, "b")
		}
	})

	t.Run("errors when maps exist but none is default", func(t *testing.T) {
		records := []*MapRecordInfo{
			mk("main", false),
			mk("office", false),
		}
		_, err := selectDefaultMap(records)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "is_default") {
			t.Fatalf("error should mention is_default, got: %v", err)
		}
	})

	t.Run("returns empty string for empty records", func(t *testing.T) {
		got, err := selectDefaultMap(nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "" {
			t.Fatalf("got %q, want empty string", got)
		}
	})

	// Sanity: MapRecordInfo carries Options without breaking selection.
	t.Run("ignores options field", func(t *testing.T) {
		records := []*MapRecordInfo{
			{Name: "x", IsDefault: true, Options: json.RawMessage(`{"foo":1}`)},
		}
		got, err := selectDefaultMap(records)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "x" {
			t.Fatalf("got %q, want %q", got, "x")
		}
	})
}
