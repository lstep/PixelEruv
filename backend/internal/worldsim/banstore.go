package worldsim

import (
	"fmt"
	"time"

	"github.com/pocketbase/pocketbase/core"
)

// Ban represents a record in PocketBase's bans collection.
type Ban struct {
	TargetType  string // "user_id", "ip", or "device_id"
	TargetValue string
	Reason      string
	BannedUntil int64  // unix timestamp; 0 = permanent
}

// IsActive returns true if the ban is still in effect at the given unix time.
func (b *Ban) IsActive(now int64) bool {
	return b.BannedUntil == 0 || b.BannedUntil > now
}

// BanTargetType constants identify which identifier a ban targets.
const (
	BanTargetUserID   = "user_id"
	BanTargetIP       = "ip"
	BanTargetDeviceID = "device_id"
)

// MatchesActive returns the first active ban in the list that matches any of
// the given identifiers, or nil if none match. Empty identifiers are never
// matched (a ban on "" would block everyone). This is a pure function so it
// can be unit-tested without PocketBase.
func MatchesActive(bans []Ban, sub, ip, deviceID string, now int64) *Ban {
	for i := range bans {
		b := &bans[i]
		if !b.IsActive(now) {
			continue
		}
		switch b.TargetType {
		case BanTargetUserID:
			if sub != "" && b.TargetValue == sub {
				return b
			}
		case BanTargetIP:
			if ip != "" && b.TargetValue == ip {
				return b
			}
		case BanTargetDeviceID:
			if deviceID != "" && b.TargetValue == deviceID {
				return b
			}
		}
	}
	return nil
}

// BanStore handles PocketBase ban lookups via the Go SDK (in-process DAO
// access, no HTTP). Mirrors the UserStore pattern.
type BanStore struct {
	app core.App
}

func NewBanStore(app core.App) *BanStore {
	return &BanStore{app: app}
}

// CheckBan queries the bans collection for active bans matching any of the
// three identifiers. Returns the first active match, or nil if none found.
func (s *BanStore) CheckBan(sub, ip, deviceID string) (*Ban, bool) {
	now := time.Now().Unix()

	for _, tt := range []struct {
		targetType string
		value      string
	}{
		{BanTargetUserID, sub},
		{BanTargetIP, ip},
		{BanTargetDeviceID, deviceID},
	} {
		if tt.value == "" {
			continue
		}
		record, err := s.app.FindFirstRecordByFilter(
			"bans",
			"target_type = {:type} && target_value = {:value}",
			map[string]any{"type": tt.targetType, "value": tt.value},
		)
		if err != nil || record == nil {
			continue
		}
		ban := recordToBan(record)
		if ban.IsActive(now) {
			return &ban, true
		}
	}
	return nil, false
}

func recordToBan(r *core.Record) Ban {
	return Ban{
		TargetType:  r.GetString("target_type"),
		TargetValue: r.GetString("target_value"),
		Reason:      r.GetString("reason"),
		BannedUntil: int64(r.GetInt("banned_until")),
	}
}

// FormatBanExpiry returns a human-readable expiry string for a ban.
func FormatBanExpiry(banUntil int64) string {
	if banUntil == 0 {
		return "permanently"
	}
	return fmt.Sprintf("until %s", time.Unix(banUntil, 0).Format(time.RFC1123))
}
