package worldsim

import (
	"testing"
)

func TestBan_IsActive(t *testing.T) {
	tests := []struct {
		name        string
		bannedUntil int64
		now         int64
		want        bool
	}{
		{"permanent ban (0)", 0, 1000, true},
		{"active temporary ban", 2000, 1000, true},
		{"expired ban", 500, 1000, false},
		{"ban expires now", 1000, 1000, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := &Ban{BannedUntil: tt.bannedUntil}
			if got := b.IsActive(tt.now); got != tt.want {
				t.Errorf("IsActive(%d) = %v, want %v", tt.now, got, tt.want)
			}
		})
	}
}

func TestMatchesActive(t *testing.T) {
	now := int64(1000)
	bans := []Ban{
		{TargetType: BanTargetOidcSub, TargetValue: "user-123", Reason: "spam", BannedUntil: 0},
		{TargetType: BanTargetIP, TargetValue: "1.2.3.4", Reason: "griefing", BannedUntil: 2000},
		{TargetType: BanTargetDeviceID, TargetValue: "dev-abc", Reason: "trolling", BannedUntil: 0},
		{TargetType: BanTargetIP, TargetValue: "5.6.7.8", Reason: "expired", BannedUntil: 500},
	}

	tests := []struct {
		name     string
		sub      string
		ip       string
		deviceID string
		wantIdx  int // index into bans, -1 for no match
	}{
		{"match by sub", "user-123", "9.9.9.9", "dev-xyz", 0},
		{"match by ip", "other", "1.2.3.4", "dev-xyz", 1},
		{"match by device_id", "other", "9.9.9.9", "dev-abc", 2},
		{"expired ban skipped", "other", "5.6.7.8", "dev-xyz", -1},
		{"no match", "other", "9.9.9.9", "dev-xyz", -1},
		{"empty sub not matched", "", "9.9.9.9", "dev-xyz", -1},
		{"empty ip not matched", "other", "", "dev-xyz", -1},
		{"empty device not matched", "other", "9.9.9.9", "", -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MatchesActive(bans, tt.sub, tt.ip, tt.deviceID, now)
			if tt.wantIdx == -1 {
				if got != nil {
					t.Errorf("MatchesActive() = %v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("MatchesActive() = nil, want ban %d", tt.wantIdx)
			}
			if got.TargetValue != bans[tt.wantIdx].TargetValue {
				t.Errorf("MatchesActive() matched %q, want %q", got.TargetValue, bans[tt.wantIdx].TargetValue)
			}
		})
	}
}

func TestMatchesActive_EmptyList(t *testing.T) {
	if got := MatchesActive(nil, "sub", "1.2.3.4", "dev", 1000); got != nil {
		t.Errorf("MatchesActive(nil) = %v, want nil", got)
	}
}

func TestMatchesActive_FirstActiveWins(t *testing.T) {
	now := int64(1000)
	bans := []Ban{
		{TargetType: BanTargetOidcSub, TargetValue: "same-sub", Reason: "first", BannedUntil: 0},
		{TargetType: BanTargetOidcSub, TargetValue: "same-sub", Reason: "second", BannedUntil: 0},
	}
	got := MatchesActive(bans, "same-sub", "", "", now)
	if got == nil {
		t.Fatal("MatchesActive() = nil, want a ban")
	}
	if got.Reason != "first" {
		t.Errorf("MatchesActive() reason = %q, want %q (first active ban)", got.Reason, "first")
	}
}

func TestFormatBanExpiry(t *testing.T) {
	if got := FormatBanExpiry(0); got != "permanently" {
		t.Errorf("FormatBanExpiry(0) = %q, want %q", got, "permanently")
	}
	// Non-zero should contain "until"
	if got := FormatBanExpiry(2000000000); len(got) < 6 || got[:6] != "until " {
		t.Errorf("FormatBanExpiry(non-zero) = %q, want prefix %q", got, "until ")
	}
}
