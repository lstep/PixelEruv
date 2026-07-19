package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
)

// WorldsimClient is a thin NATS request-reply wrapper for the worldsim
// subjects the MCP server exposes. Each method maps to one worldsim subject;
// errors from worldsim (returned as JSON {"error":...}) are surfaced as Go
// errors so tool handlers can return them to the MCP client.
type WorldsimClient struct {
	nc    *nats.Conn
	actor string // actor.extension stamped on admin action requests (e.g. "mcp")
}

func NewWorldsimClient(nc *nats.Conn, actor string) *WorldsimClient {
	return &WorldsimClient{nc: nc, actor: actor}
}

// requestReply is the generic NATS request-reply wrapper. Returns the raw
// reply bytes, or an error if the request times out or worldsim returns an
// error payload.
func (w *WorldsimClient) requestReply(ctx context.Context, subject string, payload any) ([]byte, error) {
	var data []byte
	if payload != nil {
		var err error
		data, err = json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("marshal %s payload: %w", subject, err)
		}
	}
	// Use the context deadline if set, else a sane default.
	deadline, ok := ctx.Deadline()
	timeout := 5 * time.Second
	if ok {
		timeout = time.Until(deadline)
		if timeout <= 0 {
			timeout = 5 * time.Second
		}
	}
	reply, err := w.nc.RequestWithContext(ctx, subject, data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", subject, err)
	}
	// Check for a JSON error payload (used by read handlers).
	var errResp struct {
		Error string `json:"error,omitempty"`
	}
	if err := json.Unmarshal(reply.Data, &errResp); err == nil && errResp.Error != "" {
		return nil, fmt.Errorf("%s: %s", subject, errResp.Error)
	}
	return reply.Data, nil
}

// adminActor returns the audit.Actor to stamp on admin action requests, so
// worldsim audit events attribute the action to the MCP server.
func (w *WorldsimClient) adminActor() map[string]any {
	return map[string]any{"extension": w.actor}
}

// --- Read methods ---

// StatsResponse mirrors worldsim.statsResponse (stats.go). Re-declared here
// so the MCP server doesn't depend on the worldsim package internals.
type StatsResponse struct {
	TickHz        int            `json:"tick_hz"`
	TickCount     uint64         `json:"tick_count"`
	Uptime        string         `json:"uptime"`
	TotalEntities int            `json:"total_entities"`
	TotalPlayers  int            `json:"total_players"`
	Maps          []MapStats     `json:"maps"`
	Players       []PlayerStats  `json:"players"`
	Extensions    []ExtStats     `json:"extensions"`
}

type MapStats struct {
	Name        string     `json:"name"`
	Width       int        `json:"width"`
	Height      int        `json:"height"`
	PlayerCount int        `json:"player_count"`
	EntityCount int        `json:"entity_count"`
	ZoneCount   int        `json:"zone_count"`
	SpawnZones  int        `json:"spawn_zones"`
	Zones       []ZoneStats `json:"zones"`
}

type ZoneStats struct {
	ID           string `json:"id"`
	ZoneType     string `json:"zone_type"`
	Shape        string `json:"shape"`
	AvEnabled    bool   `json:"av_enabled"`
	IsExclusive  bool   `json:"is_exclusive"`
	PortalTarget string `json:"portal_target,omitempty"`
	Occupancy    int    `json:"occupancy"`
}

type PlayerStats struct {
	EntityID    string  `json:"entity_id"`
	ClientID    string  `json:"client_id"`
	DisplayName string  `json:"display_name"`
	MapID       string  `json:"map_id"`
	X           float32 `json:"x"`
	Y           float32 `json:"y"`
	IsAdmin     bool    `json:"is_admin"`
	IsGuest     bool    `json:"is_guest"`
	IP          string  `json:"ip,omitempty"`
}

type ExtStats struct {
	ID            string `json:"id"`
	HeartbeatAge  string `json:"heartbeat_age"`
	Alive         bool   `json:"alive"`
	InputTriggers int    `json:"input_triggers"`
	GateTriggers  int    `json:"gate_triggers"`
}

func (w *WorldsimClient) GetStats(ctx context.Context) (*StatsResponse, error) {
	data, err := w.requestReply(ctx, "worldsim.stats.get", nil)
	if err != nil {
		return nil, err
	}
	var stats StatsResponse
	if err := json.Unmarshal(data, &stats); err != nil {
		return nil, fmt.Errorf("unmarshal stats: %w", err)
	}
	return &stats, nil
}

// ZoneMetadataResponse mirrors worldsim.zoneMetadataMsg (zonemeta.go).
type ZoneMetadataResponse struct {
	Maps map[string][]ZoneMeta `json:"maps"`
}

type ZoneMeta struct {
	ID           string `json:"id"`
	ZoneType     string `json:"zone_type"`
	Shape        string `json:"shape"`
	AvEnabled    bool   `json:"av_enabled"`
	IsExclusive  bool   `json:"is_exclusive"`
	PortalTarget string `json:"portal_target,omitempty"`
}

func (w *WorldsimClient) GetZones(ctx context.Context) (*ZoneMetadataResponse, error) {
	data, err := w.requestReply(ctx, "worldsim.zones.get", nil)
	if err != nil {
		return nil, err
	}
	var resp ZoneMetadataResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal zones: %w", err)
	}
	return &resp, nil
}

// EntitySnapshot mirrors worldsim.entitySnapshot (entities_query.go).
type EntitySnapshot struct {
	EntityID       string  `json:"entity_id"`
	EntityType     string  `json:"entity_type,omitempty"`
	OwnerExtension string  `json:"owner_extension,omitempty"`
	MapID          string  `json:"map_id"`
	X              float32 `json:"x"`
	Y              float32 `json:"y"`
	IsPlayer       bool    `json:"is_player"`
	DisplayName    string  `json:"display_name,omitempty"`
	IsAdmin        bool    `json:"is_admin,omitempty"`
	IsGuest        bool    `json:"is_guest,omitempty"`
	SpriteBase     string  `json:"sprite_base,omitempty"`
	Status         uint32  `json:"status,omitempty"`
	State          string  `json:"state,omitempty"`
	Gid            uint32  `json:"gid,omitempty"`
	GidOff         uint32  `json:"gid_off,omitempty"`
	GidOn          uint32  `json:"gid_on,omitempty"`
	LightIntensity uint32  `json:"light_intensity,omitempty"`
	LightColor     uint32  `json:"light_color,omitempty"`
	LightRadius    float32 `json:"light_radius,omitempty"`
}

type EntitiesQuery struct {
	MapID          string `json:"map_id,omitempty"`
	EntityType     string `json:"entity_type,omitempty"`
	OwnerExtension string `json:"owner_extension,omitempty"`
	ZoneID         string `json:"zone_id,omitempty"`
	Limit          int    `json:"limit,omitempty"`
}

func (w *WorldsimClient) QueryEntities(ctx context.Context, q EntitiesQuery) ([]EntitySnapshot, error) {
	data, err := w.requestReply(ctx, "worldsim.entities.query", q)
	if err != nil {
		return nil, err
	}
	var out []EntitySnapshot
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("unmarshal entities: %w", err)
	}
	return out, nil
}

func (w *WorldsimClient) GetEntity(ctx context.Context, entityID string) (*EntitySnapshot, error) {
	data, err := w.requestReply(ctx, "worldsim.entity.get", map[string]string{"entity_id": entityID})
	if err != nil {
		return nil, err
	}
	var snap EntitySnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, fmt.Errorf("unmarshal entity: %w", err)
	}
	return &snap, nil
}

// --- Control methods ---

// AdminResponse mirrors worldsim.adminResponse / banResponse.
type AdminResponse struct {
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
	Kicked bool   `json:"kicked,omitempty"`
	Reason string `json:"reason,omitempty"`
}

func (w *WorldsimClient) Teleport(ctx context.Context, entityID, mapID, targetEntity string) (*AdminResponse, error) {
	payload := map[string]any{
		"entity_id":     entityID,
		"map_id":        mapID,
		"target_entity": targetEntity,
	}
	// worldsim.entity.teleport is fire-and-forget (no reply). Publish it and
	// return a synthetic success. (If a future worldsim version adds a reply,
	// we can switch to requestReply.)
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	if err := w.nc.Publish("worldsim.entity.teleport", data); err != nil {
		return nil, fmt.Errorf("teleport publish: %w", err)
	}
	return &AdminResponse{OK: true}, nil
}

func (w *WorldsimClient) Kick(ctx context.Context, clientID, reason string) (*AdminResponse, error) {
	payload := map[string]any{
		"client_id": clientID,
		"reason":    reason,
		"actor":     w.adminActor(),
	}
	data, err := w.requestReply(ctx, "worldsim.client.kick", payload)
	if err != nil {
		return nil, err
	}
	var resp AdminResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal kick response: %w", err)
	}
	return &resp, nil
}

func (w *WorldsimClient) Ban(ctx context.Context, targetType, targetValue, reason string, bannedUntil int64, bannedBy string) (*AdminResponse, error) {
	payload := map[string]any{
		"target_type":  targetType,
		"target_value": targetValue,
		"reason":       reason,
		"banned_until": bannedUntil,
		"banned_by":    bannedBy,
		"actor":        w.adminActor(),
	}
	data, err := w.requestReply(ctx, "worldsim.client.ban", payload)
	if err != nil {
		return nil, err
	}
	var resp AdminResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal ban response: %w", err)
	}
	return &resp, nil
}

func (w *WorldsimClient) SendChatAs(ctx context.Context, entityID, channel, text string) (*AdminResponse, error) {
	payload := map[string]any{
		"entity_id": entityID,
		"channel":   channel,
		"text":      text,
		"actor":     w.adminActor(),
	}
	data, err := w.requestReply(ctx, "worldsim.admin.chat", payload)
	if err != nil {
		return nil, err
	}
	var resp AdminResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal chat response: %w", err)
	}
	return &resp, nil
}

func (w *WorldsimClient) SetName(ctx context.Context, entityID, name string) (*AdminResponse, error) {
	payload := map[string]any{
		"entity_id": entityID,
		"name":      name,
		"actor":     w.adminActor(),
	}
	data, err := w.requestReply(ctx, "worldsim.admin.set_name", payload)
	if err != nil {
		return nil, err
	}
	var resp AdminResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal set_name response: %w", err)
	}
	return &resp, nil
}

func (w *WorldsimClient) SetStatus(ctx context.Context, entityID string, status uint32) (*AdminResponse, error) {
	payload := map[string]any{
		"entity_id": entityID,
		"status":    status,
		"actor":     w.adminActor(),
	}
	data, err := w.requestReply(ctx, "worldsim.admin.set_status", payload)
	if err != nil {
		return nil, err
	}
	var resp AdminResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal set_status response: %w", err)
	}
	return &resp, nil
}

func (w *WorldsimClient) SetSprite(ctx context.Context, entityID, spriteBase string) (*AdminResponse, error) {
	payload := map[string]any{
		"entity_id":   entityID,
		"sprite_base": spriteBase,
		"actor":       w.adminActor(),
	}
	data, err := w.requestReply(ctx, "worldsim.admin.set_sprite", payload)
	if err != nil {
		return nil, err
	}
	var resp AdminResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal set_sprite response: %w", err)
	}
	return &resp, nil
}

func (w *WorldsimClient) SetPlayerOptions(ctx context.Context, entityID, options string) (*AdminResponse, error) {
	payload := map[string]any{
		"entity_id": entityID,
		"options":   options,
		"actor":     w.adminActor(),
	}
	data, err := w.requestReply(ctx, "worldsim.admin.set_player_options", payload)
	if err != nil {
		return nil, err
	}
	var resp AdminResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal set_player_options response: %w", err)
	}
	return &resp, nil
}

// DispatchExtensionAction publishes extension.<id>.action without waiting for
// a reply. Used by the MCP dispatch_extension_action tool. The caller has no
// way to know if the extension handled it; for that, use the audit log.
func (w *WorldsimClient) DispatchExtensionAction(ctx context.Context, extID string, payload map[string]any) error {
	subject := fmt.Sprintf("extension.%s.action", extID)
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal action payload: %w", err)
	}
	return w.nc.Publish(subject, data)
}

// --- World options ---

// WorldOptions mirrors worldsim.WorldOptions (worldoptions.go). Re-declared
// here so the MCP server doesn't depend on the worldsim package internals.
// FFmpegTimeout is int64 nanoseconds on the wire (Go time.Duration marshals
// to ns); 1 minute = 60000000000.
type WorldOptions struct {
	SMTPHost                  string `json:"smtp_host"`
	SMTPPort                  int    `json:"smtp_port"`
	SMTPUsername              string `json:"smtp_username"`
	SMTPPassword              string `json:"smtp_password"`
	SMTPFrom                  string `json:"smtp_from"`
	SMTPSender                string `json:"smtp_sender_name"`
	SMTPTLS                   bool   `json:"smtp_tls"`
	AppURL                    string `json:"app_url"`
	YoutubeRTMPURL            string `json:"youtube_rtmp_url"`
	YoutubeStreamKey          string `json:"youtube_stream_key"`
	FFmpegConcurrency         int    `json:"ffmpeg_concurrency"`
	FFmpegTimeout             int64  `json:"ffmpeg_timeout"` // nanoseconds
	KingName                  string `json:"king_name"`
	KingEmail                 string `json:"king_email"`
	ErrorEmailRecipientsMode  string `json:"error_email_recipients_mode"`
	ErrorEmailCustomAddresses string `json:"error_email_custom_addresses"`
	RecordingEnabled          bool   `json:"recording_enabled"`
	PublicHost                string `json:"public_host"`         // readOnly
	LivekitPublicURL          string `json:"livekit_public_url"`  // readOnly
}

// worldOptionsReply mirrors worldsim.worldOptionsReply.
type worldOptionsReply struct {
	OK      bool         `json:"ok"`
	Error   string       `json:"error,omitempty"`
	Options WorldOptions `json:"options,omitempty"`
}

// GetWorldOptions calls worldsim.world_options.get and returns the current
// server-wide runtime config (SMTP, AppURL, YouTube RTMP, ffmpeg limits,
// world king, error-email recipients, recording gate, readOnly env mirrors).
func (w *WorldsimClient) GetWorldOptions(ctx context.Context) (*WorldOptions, error) {
	data, err := w.requestReply(ctx, "worldsim.world_options.get", nil)
	if err != nil {
		return nil, err
	}
	var resp worldOptionsReply
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal world_options.get: %w", err)
	}
	if !resp.OK {
		return nil, fmt.Errorf("world_options.get: %s", resp.Error)
	}
	return &resp.Options, nil
}

// SetWorldOptions calls worldsim.world_options.set with a full WorldOptions
// payload and an actor tag identifying the MCP server. worldsim validates,
// writes the NATS KV bucket, broadcasts world_options.update so consumers
// (worldsim SMTP, ext-rec ffmpeg/YouTube, frontend recording gate)
// hot-reload, and emits a world_options.updated audit event attributed to
// actor.extension=w.actor (default "mcp"). readOnly fields (PublicHost,
// LivekitPublicURL) in opts are ignored — worldsim preserves them from the
// current value. Returns the post-write options on success.
func (w *WorldsimClient) SetWorldOptions(ctx context.Context, opts WorldOptions) (*WorldOptions, error) {
	payload := map[string]any{
		"options": opts,
		"actor":   w.adminActor(),
	}
	data, err := w.requestReply(ctx, "worldsim.world_options.set", payload)
	if err != nil {
		return nil, err
	}
	var resp worldOptionsReply
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal world_options.set: %w", err)
	}
	if !resp.OK {
		return nil, fmt.Errorf("world_options.set: %s", resp.Error)
	}
	return &resp.Options, nil
}
