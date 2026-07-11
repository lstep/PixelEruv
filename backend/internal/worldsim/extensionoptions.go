package worldsim

import (
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/nats-io/nats.go"
	"github.com/pocketbase/pocketbase/core"
)

// optionFieldDef is a single option declared by an extension in its
// registration message. Type is one of: "bool", "number", "text".
// Default is the default value (JSON-encoded).
type optionFieldDef struct {
	Name    string          `json:"name"`
	Type    string          `json:"type"`
	Default json.RawMessage `json:"default"`
}

// optionsSchemaMsg is the options schema portion of the registration message.
// Sent as the "options_schema" field in the register payload.
type optionsSchemaMsg struct {
	Options []optionFieldDef `json:"options"`
}

// ExtensionOptionsManager handles extension options: PB collection creation,
// default value seeding, and NATS-based option delivery to extensions.
type ExtensionOptionsManager struct {
	app    core.App
	nc     *nats.Conn
	logger *slog.Logger
}

func NewExtensionOptionsManager(app core.App, nc *nats.Conn, logger *slog.Logger) *ExtensionOptionsManager {
	return &ExtensionOptionsManager{
		app:    app,
		nc:     nc,
		logger: logger,
	}
}

// EnsureOptions ensures a row exists in the extension_options collection for
// the given extension, creating one with default values if missing. It also
// backfills any fields declared in the schema that are absent from the stored
// options JSON. It returns the current options JSON.
func (m *ExtensionOptionsManager) EnsureOptions(extensionID string, schema []optionFieldDef) (json.RawMessage, error) {
	if m.app == nil {
		return nil, fmt.Errorf("no app")
	}

	collection, err := m.app.FindCollectionByNameOrId("extension_options")
	if err != nil {
		return nil, fmt.Errorf("find extension_options collection: %w", err)
	}

	// Look for an existing row for this extension.
	record, _ := m.app.FindFirstRecordByData(collection, "extension_id", extensionID)
	if record == nil {
		// Create a new row with default values.
		defaults := buildDefaultsJSON(schema)
		record = core.NewRecord(collection)
		record.Set("extension_id", extensionID)
		record.Set("options", string(defaults))
		if err := m.app.Save(record); err != nil {
			return nil, fmt.Errorf("create extension_options row: %w", err)
		}
		m.logger.Info("created extension options row", "extension", extensionID, "defaults", string(defaults))
		return defaults, nil
	}

	// Backfill any missing fields from the schema.
	raw := record.GetString("options")
	var current map[string]json.RawMessage
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &current); err != nil {
			current = make(map[string]json.RawMessage)
		}
	}
	changed := false
	if current == nil {
		current = make(map[string]json.RawMessage)
	}
	for _, f := range schema {
		if _, ok := current[f.Name]; !ok {
			current[f.Name] = f.Default
			changed = true
		}
	}
	if changed {
		updated, _ := json.Marshal(current)
		record.Set("options", string(updated))
		if err := m.app.Save(record); err != nil {
			return nil, fmt.Errorf("backfill extension_options: %w", err)
		}
		m.logger.Info("backfilled extension options", "extension", extensionID)
		raw = string(updated)
	}

	return json.RawMessage(raw), nil
}

// PublishOptions sends the current options for an extension via NATS on
// extension.<id>.options. Called after registration and on PB record change.
func (m *ExtensionOptionsManager) PublishOptions(extensionID string) {
	if m.app == nil || m.nc == nil {
		return
	}
	collection, err := m.app.FindCollectionByNameOrId("extension_options")
	if err != nil {
		return
	}
	record, _ := m.app.FindFirstRecordByData(collection, "extension_id", extensionID)
	if record == nil {
		return
	}
	raw := record.GetString("options")
	if raw == "" {
		raw = "{}"
	}
	subject := fmt.Sprintf("extension.%s.options", extensionID)
	if err := m.nc.Publish(subject, []byte(raw)); err != nil {
		m.logger.Warn("publish extension options", "err", err, "extension", extensionID)
	}
}

// PublishAllOptions publishes options for all extensions that have a row in
// the extension_options collection. Called on worldsim.ready.
func (m *ExtensionOptionsManager) PublishAllOptions() {
	if m.app == nil || m.nc == nil {
		return
	}
	collection, err := m.app.FindCollectionByNameOrId("extension_options")
	if err != nil {
		return
	}
	records, err := m.app.FindRecordsByFilter(collection, "1=1", "", 0, 0)
	if err != nil {
		return
	}
	for _, record := range records {
		extID := record.GetString("extension_id")
		if extID == "" {
			continue
		}
		m.PublishOptions(extID)
	}
}

// buildDefaultsJSON constructs a JSON object from the schema's default values.
func buildDefaultsJSON(schema []optionFieldDef) json.RawMessage {
	m := make(map[string]json.RawMessage, len(schema))
	for _, f := range schema {
		if f.Default != nil {
			m[f.Name] = f.Default
		} else {
			// Fallback per type if no default provided.
			switch f.Type {
			case "bool":
				m[f.Name] = json.RawMessage("false")
			case "number":
				m[f.Name] = json.RawMessage("0")
			default:
				m[f.Name] = json.RawMessage(`""`)
			}
		}
	}
	data, _ := json.Marshal(m)
	return data
}
