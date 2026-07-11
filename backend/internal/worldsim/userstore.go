package worldsim

import (
	"fmt"

	"github.com/pocketbase/pocketbase/core"
)

// UserRecord represents a player in PocketBase's players collection.
type UserRecord struct {
	ID          string
	OidcSub     string
	EntityID    string
	DisplayName string
	PosX        float32
	PosY        float32
	SpriteBase  string
	MapID       string
}

// UserStore handles PocketBase player lookups and persistence via the
// PocketBase Go SDK (in-process DAO access, no HTTP).
type UserStore struct {
	app core.App
}

func NewUserStore(app core.App) *UserStore {
	return &UserStore{app: app}
}

// FindOrCreateUser looks up a player by oidc_sub. If not found, creates a new
// record with a generated entity_id and default spawn position.
func (s *UserStore) FindOrCreateUser(sub, entityID, defaultMapID string) (*UserRecord, error) {
	user, err := s.findBySub(sub)
	if err != nil {
		return nil, fmt.Errorf("find user: %w", err)
	}
	if user != nil {
		return user, nil
	}

	user = &UserRecord{
		OidcSub:  sub,
		EntityID: entityID,
		PosX:     10,
		PosY:     10,
		MapID:    defaultMapID,
	}
	if err := s.create(user); err != nil {
		return nil, fmt.Errorf("create user: %w", err)
	}
	return user, nil
}

// SavePosition updates the player's position in PocketBase.
func (s *UserStore) SavePosition(entityID string, x, y float32) error {
	record, err := s.findByEntityIDRecord(entityID)
	if err != nil {
		return fmt.Errorf("find user for save: %w", err)
	}
	if record == nil {
		return nil
	}

	record.Set("pos_x", x)
	record.Set("pos_y", y)
	return s.app.Save(record)
}

// SaveMapID updates the player's current map in PocketBase.
func (s *UserStore) SaveMapID(entityID, mapID string) error {
	record, err := s.findByEntityIDRecord(entityID)
	if err != nil {
		return fmt.Errorf("find user for map_id save: %w", err)
	}
	if record == nil {
		return nil
	}

	record.Set("map_id", mapID)
	return s.app.Save(record)
}

// UpdateDisplayName persists the player's display name to PocketBase. No-op
// if the entity has no PocketBase record (guests).
func (s *UserStore) UpdateDisplayName(entityID, name string) error {
	record, err := s.findByEntityIDRecord(entityID)
	if err != nil {
		return fmt.Errorf("find user for name update: %w", err)
	}
	if record == nil {
		return nil
	}

	record.Set("display_name", name)
	return s.app.Save(record)
}

// UpdateSpriteBase persists the player's chosen sprite_bases record ID to
// PocketBase. No-op if the entity has no PocketBase record (guests).
func (s *UserStore) UpdateSpriteBase(entityID, spriteBase string) error {
	record, err := s.findByEntityIDRecord(entityID)
	if err != nil {
		return fmt.Errorf("find user for sprite_base update: %w", err)
	}
	if record == nil {
		return nil
	}

	record.Set("sprite_base", spriteBase)
	return s.app.Save(record)
}

func (s *UserStore) findBySub(sub string) (*UserRecord, error) {
	record, err := s.app.FindFirstRecordByData("players", "oidc_sub", sub)
	if err != nil {
		return nil, nil // not found is not an error here
	}
	if record == nil {
		return nil, nil
	}
	return recordToUser(record), nil
}

// findByEntityIDRecord returns the raw *core.Record for update operations.
func (s *UserStore) findByEntityIDRecord(entityID string) (*core.Record, error) {
	record, err := s.app.FindFirstRecordByData("players", "entity_id", entityID)
	if err != nil {
		return nil, nil
	}
	return record, nil
}

func (s *UserStore) create(user *UserRecord) error {
	collection, err := s.app.FindCollectionByNameOrId("players")
	if err != nil {
		return fmt.Errorf("find players collection: %w", err)
	}

	record := core.NewRecord(collection)
	record.Set("oidc_sub", user.OidcSub)
	record.Set("entity_id", user.EntityID)
	record.Set("pos_x", user.PosX)
	record.Set("pos_y", user.PosY)
	if user.MapID != "" {
		record.Set("map_id", user.MapID)
	}
	if err := s.app.Save(record); err != nil {
		return err
	}

	user.ID = record.Id
	return nil
}

func recordToUser(r *core.Record) *UserRecord {
	return &UserRecord{
		ID:          r.Id,
		OidcSub:     r.GetString("oidc_sub"),
		EntityID:    r.GetString("entity_id"),
		DisplayName: r.GetString("display_name"),
		PosX:        float32(r.GetFloat("pos_x")),
		PosY:        float32(r.GetFloat("pos_y")),
		SpriteBase:  r.GetString("sprite_base"),
		MapID:       r.GetString("map_id"),
	}
}
