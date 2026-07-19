package worldsim

import (
	"fmt"
	"time"

	"github.com/pocketbase/pocketbase/core"
)

// UserRecord represents a player in PocketBase's players collection.
type UserRecord struct {
	ID          string
	UserID      string // PocketBase users collection record ID
	EntityID    string
	DisplayName string
	PosX        float32
	PosY        float32
	SpriteBase  string
	MapID       string
	IP          string
	LastSeenAt  int64
	IsAdmin     bool
	Options     string
	HideAdminBadge bool // players.hide_admin_badge — opt out of the public admin badge
	Status     uint32 // players.status — presence status (0=AVAILABLE,1=BUSY,2=DND), persisted across sessions
}

// UserStore handles PocketBase player lookups and persistence via the
// PocketBase Go SDK (in-process DAO access, no HTTP).
type UserStore struct {
	app core.App
}

func NewUserStore(app core.App) *UserStore {
	return &UserStore{app: app}
}

// FindOrCreateUser looks up a player by user_id. If not found, creates a new
// record with a generated entity_id and default spawn position. On both create
// and reconnect, the client IP and last_seen_at timestamp are persisted.
func (s *UserStore) FindOrCreateUser(sub, entityID, defaultMapID, ip string) (*UserRecord, error) {
	user, err := s.findByUserID(sub)
	if err != nil {
		return nil, fmt.Errorf("find user: %w", err)
	}
	if user != nil {
		if err := s.UpdateConnectInfo(user.EntityID, ip); err != nil {
			return nil, fmt.Errorf("update connect info: %w", err)
		}
		user.IP = ip
		user.LastSeenAt = time.Now().Unix()
		return user, nil
	}

	user = &UserRecord{
		UserID:     sub,
		EntityID:   entityID,
		PosX:       10,
		PosY:       10,
		MapID:      defaultMapID,
		IP:         ip,
		LastSeenAt: time.Now().Unix(),
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

// UpdateOptions persists the player's options JSON to PocketBase. No-op if the
// entity has no PocketBase record (guests — session-only options).
func (s *UserStore) UpdateOptions(entityID, options string) error {
	record, err := s.findByEntityIDRecord(entityID)
	if err != nil {
		return fmt.Errorf("find user for options update: %w", err)
	}
	if record == nil {
		return nil
	}

	record.Set("options", options)
	return s.app.Save(record)
}

// UpdateStatus persists the player's presence status to PocketBase. No-op if
// the entity has no PocketBase record (guests — session-only status).
func (s *UserStore) UpdateStatus(entityID string, status uint32) error {
	record, err := s.findByEntityIDRecord(entityID)
	if err != nil {
		return fmt.Errorf("find user for status update: %w", err)
	}
	if record == nil {
		return nil
	}

	record.Set("status", status)
	return s.app.Save(record)
}

// UpdateConnectInfo persists the client IP and last_seen_at timestamp for a
// player. Called on every connect. No-op if the entity has no PocketBase
// record (guests).
func (s *UserStore) UpdateConnectInfo(entityID, ip string) error {
	record, err := s.findByEntityIDRecord(entityID)
	if err != nil {
		return fmt.Errorf("find user for connect info update: %w", err)
	}
	if record == nil {
		return nil
	}

	record.Set("ip", ip)
	record.Set("last_seen_at", time.Now().Unix())
	return s.app.Save(record)
}

func (s *UserStore) findByUserID(userID string) (*UserRecord, error) {
	record, err := s.app.FindFirstRecordByData("players", "user_id", userID)
	if err != nil {
		return nil, nil // not found is not an error here
	}
	if record == nil {
		return nil, nil
	}
	return recordToUser(record), nil
}

// IsAdmin returns true if the player linked to the given users-collection
// record ID has is_admin=true. Returns false if the player record is missing
// or on error. Used by the /api/world-options HTTP handler to gate access
// to the YouTube RTMP defaults.
func (s *UserStore) IsAdmin(userID string) (bool, error) {
	user, err := s.findByUserID(userID)
	if err != nil {
		return false, err
	}
	if user == nil {
		return false, nil
	}
	return user.IsAdmin, nil
}

// AdminEmails returns the email addresses of every user linked to a players
// row with is_admin=true. Used by the audit service's error-email notifier
// when error_email_recipients_mode == "all_admins" (resolved via the
// worldsim.admin_emails.get NATS request-reply). Returns de-duplicated
// emails; users without an email field are skipped.
func (s *UserStore) AdminEmails() ([]string, error) {
	records, err := s.app.FindRecordsByFilter("players", "is_admin=true", "", 0, 0)
	if err != nil {
		return nil, fmt.Errorf("query admin players: %w", err)
	}
	var out []string
	seen := map[string]bool{}
	for _, r := range records {
		uid := r.GetString("user_id")
		if uid == "" {
			continue
		}
		user, err := s.app.FindRecordById("users", uid)
		if err != nil || user == nil {
			continue
		}
		email := user.GetString("email")
		if email == "" || seen[email] {
			continue
		}
		seen[email] = true
		out = append(out, email)
	}
	return out, nil
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
	record.Set("user_id", user.UserID)
	record.Set("entity_id", user.EntityID)
	record.Set("pos_x", user.PosX)
	record.Set("pos_y", user.PosY)
	if user.MapID != "" {
		record.Set("map_id", user.MapID)
	}
	if user.IP != "" {
		record.Set("ip", user.IP)
	}
	if user.LastSeenAt != 0 {
		record.Set("last_seen_at", user.LastSeenAt)
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
		UserID:      r.GetString("user_id"),
		EntityID:    r.GetString("entity_id"),
		DisplayName: r.GetString("display_name"),
		PosX:        float32(r.GetFloat("pos_x")),
		PosY:        float32(r.GetFloat("pos_y")),
		SpriteBase:  r.GetString("sprite_base"),
		MapID:       r.GetString("map_id"),
		IP:          r.GetString("ip"),
		LastSeenAt:  int64(r.GetInt("last_seen_at")),
		IsAdmin:     r.GetBool("is_admin"),
		Options:     r.GetString("options"),
		HideAdminBadge: r.GetBool("hide_admin_badge"),
		Status:     uint32(r.GetInt("status")),
	}
}
