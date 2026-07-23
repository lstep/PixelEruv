package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
)

// PBPlayer is a player record from PocketBase, fetched via the
// worldsim.players.list NATS request-reply. Only the fields needed by the
// audit players page are extracted.
type PBPlayer struct {
	UserID      string `json:"user_id"`      // OIDC sub — matches audit actor_sub
	DisplayName string `json:"display_name"`
	EntityID    string `json:"entity_id"`
	IsAdmin     bool   `json:"is_admin"`
	Created     string `json:"created"` // PocketBase ISO timestamp
}

// PlayerListClient fetches all registered players from worldsim via NATS
// (worldsim.players.list). worldsim owns PocketBase and exposes the player
// list via NATS because the players collection REST API is locked down
// (nil rules — see migration 1753900000_lock_players_collection.go).
type PlayerListClient struct {
	nc        *nats.Conn
	timeout   time.Duration
}

func NewPlayerListClient(nc *nats.Conn) *PlayerListClient {
	return &PlayerListClient{
		nc:      nc,
		timeout: 5 * time.Second,
	}
}

// ListPlayers fetches all player records via worldsim.players.list NATS
// request-reply. Returns nil if nc is nil (NATS not configured) — the caller
// falls back to audit-only data.
func (c *PlayerListClient) ListPlayers(ctx context.Context) ([]PBPlayer, error) {
	if c.nc == nil {
		return nil, nil
	}
	msg := nats.NewMsg("worldsim.players.list")
	msg.Data = nil
	reply, err := c.nc.RequestMsg(msg, c.timeout)
	if err != nil {
		return nil, fmt.Errorf("worldsim.players.list: %w", err)
	}
	var result struct {
		OK      bool       `json:"ok"`
		Error   string     `json:"error,omitempty"`
		Players []PBPlayer `json:"players"`
	}
	if err := json.Unmarshal(reply.Data, &result); err != nil {
		return nil, fmt.Errorf("unmarshal players list: %w", err)
	}
	if !result.OK {
		return nil, fmt.Errorf("worldsim.players.list: %s", result.Error)
	}
	return result.Players, nil
}
