// ext-rec is a first-party extension that records A/V meetings in LiveKit
// rooms via the LiveKit Egress service. An admin (host) sends a
// RecordingRequestFrame from the browser; the pusher forwards it as a NATS
// message on recording.start / recording.stop. ext-rec authorizes the host
// via worldsim.entity_info (admin only), starts or stops a LiveKit Room
// Composite Egress, and records the meeting in the PocketBase `recordings`
// collection. State (active Egress IDs) is in-memory and lost on restart —
// v1 mitigation TBD (orphan cleanup via ListEgress).
//
// Two mutually exclusive targets per recording, chosen at start time:
//   - "mp4":   local file under RECORDINGS_DIR (default ./recordings).
//   - "youtube": live RTMP stream to YOUTUBE_RTMP_URL/YOUTUBE_STREAM_KEY.
//
// One recording per room at a time. Proximity rooms are out of scope for v1;
// only zone A/V rooms (the rooms ext-av mints tokens for) are recorded.
//
// Subscriptions:
//   - recording.start / recording.stop: host requests (pusher → ext-rec).
//   - worldsim.ready: re-register on worldsim restart.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/lstep/pixeleruv/backend/internal/audit"
	"github.com/lstep/pixeleruv/backend/internal/otel"
	"github.com/lstep/pixeleruv/backend/internal/version"
	"github.com/nats-io/nats.go"
)

type registerMsg struct {
	ExtensionID        string           `json:"extension_id"`
	HeartbeatIntervalS int              `json:"heartbeat_interval_s"`
	OptionsSchema      []optionFieldDef `json:"options_schema,omitempty"`
}

type optionFieldDef struct {
	Name    string          `json:"name"`
	Type    string          `json:"type"`
	Default json.RawMessage `json:"default"`
}

// recordingStartMsg is the NATS payload of recording.start. The pusher adds
// client_id and entity_id from the session; room and target come from the
// RecordingRequestFrame.
type recordingStartMsg struct {
	ClientID string `json:"client_id"`
	EntityID string `json:"entity_id"`
	Room     string `json:"room"`
	Target   string `json:"target"` // "mp4" | "youtube"
}

type recordingStopMsg struct {
	ClientID string `json:"client_id"`
	EntityID string `json:"entity_id"`
	Room     string `json:"room"`
}

// entityInfoReply is the reply payload of worldsim.entity_info. Empty
// EntityID signals "not found".
type entityInfoReply struct {
	EntityID    string `json:"entity_id"`
	IsAdmin     bool   `json:"is_admin"`
	Status      uint32 `json:"status"`
	DisplayName string `json:"display_name"`
	MapID       string `json:"map_id"`
}

// recordingStateMsg is published on client.<id>.recording_state for the host.
// Mirrors the RecordingStateFrame proto fields.
type recordingStateMsg struct {
	Room     string `json:"room"`
	Status   string `json:"status"` // "active" | "stopped" | "error"
	Target   string `json:"target"`
	EgressID string `json:"egress_id,omitempty"`
	Error    string `json:"error,omitempty"`
}

// recordingActiveMsg is published on client.<id>.recording_active for each
// participant in the room. Mirrors the RecordingActiveFrame proto fields.
type recordingActiveMsg struct {
	Room   string `json:"room"`
	Active bool   `json:"active"`
	Target string `json:"target"`
}

// activeRec tracks a running Egress so recording.stop can find it.
type activeRec struct {
	EgressID     string
	Room         string
	Target       string // "mp4" | "youtube"
	StartedBy    string // entity_id
	StartedAt    time.Time
	Participants []string // snapshot at start
	MeetingID    string
}

func main() {
	startTime := time.Now()
	natsURL := envOr("NATS_URL", "nats://localhost:4222")
	extID := envOr("EXTENSION_ID", "rec")
	livekitURL := envOr("LIVEKIT_URL", "ws://localhost:7880")
	livekitAPIKey := os.Getenv("LIVEKIT_API_KEY")
	livekitAPISecret := os.Getenv("LIVEKIT_API_SECRET")
	recordingsDir := envOr("RECORDINGS_DIR", "./recordings")
	youtubeRTMPURL := os.Getenv("YOUTUBE_RTMP_URL")
	youtubeStreamKey := os.Getenv("YOUTUBE_STREAM_KEY")
	heartbeatS := 10

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	if livekitAPIKey == "" || livekitAPISecret == "" {
		logger.Error("LIVEKIT_API_KEY and LIVEKIT_API_SECRET must be set")
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	logger, otelShutdown, err := otel.Init(ctx, "ext-"+extID)
	if err != nil {
		logger.Error("otel init", "err", err)
		os.Exit(1)
	}
	defer otelShutdown(context.Background())

	nc, err := nats.Connect(natsURL,
		nats.Name("ext-"+extID),
		nats.ReconnectWait(2*time.Second),
		nats.MaxReconnects(-1),
	)
	if err != nil {
		logger.Error("nats connect", "err", err)
		os.Exit(1)
	}
	defer nc.Close()

	var mu sync.Mutex
	activeRecs := make(map[string]*activeRec) // keyed by room name

	_ = livekitURL
	_ = recordingsDir
	_ = youtubeRTMPURL
	_ = youtubeStreamKey

	// publishRecordingState publishes a client.<id>.recording_state message
	// to the host.
	publishRecordingState := func(clientID string, msg recordingStateMsg) {
		if clientID == "" {
			return
		}
		data, _ := json.Marshal(msg)
		subject := fmt.Sprintf("client.%s.recording_state", clientID)
		if err := nc.Publish(subject, data); err != nil {
			logger.Warn("recording_state publish", "client", clientID, "err", err)
		}
	}

	// publishRecordingActive publishes a client.<id>.recording_active message
	// to a single participant. Called once per participant on start/stop.
	publishRecordingActive := func(clientID string, msg recordingActiveMsg) {
		if clientID == "" {
			return
		}
		data, _ := json.Marshal(msg)
		subject := fmt.Sprintf("client.%s.recording_active", clientID)
		if err := nc.Publish(subject, data); err != nil {
			logger.Warn("recording_active publish", "client", clientID, "err", err)
		}
	}

	// fetchEntityInfo calls worldsim.entity_info to authorize the host.
	fetchEntityInfo := func(entityID string) (entityInfoReply, error) {
		var reply entityInfoReply
		reqData, _ := json.Marshal(map[string]string{"entity_id": entityID})
		msg, err := nc.Request("worldsim.entity_info", reqData, 2*time.Second)
		if err != nil {
			return reply, fmt.Errorf("entity_info request: %w", err)
		}
		if err := json.Unmarshal(msg.Data, &reply); err != nil {
			return reply, fmt.Errorf("entity_info unmarshal: %w", err)
		}
		return reply, nil
	}

	// --- recording.start handler (skeleton: validate + reject; start wired in step 5) ---
	_ = publishRecordingActive // wired in step 5
	nc.Subscribe("recording.start", func(m *nats.Msg) {
		var req recordingStartMsg
		if err := json.Unmarshal(m.Data, &req); err != nil {
			logger.Warn("recording.start unmarshal", "err", err)
			return
		}
		if req.Room == "" || req.ClientID == "" || req.EntityID == "" {
			logger.Warn("recording.start missing fields", "req", req)
			return
		}
		if req.Target != "mp4" && req.Target != "youtube" {
			publishRecordingState(req.ClientID, recordingStateMsg{
				Room: req.Room, Status: "error", Target: req.Target,
				Error: "invalid target (want mp4 or youtube)",
			})
			return
		}

		info, err := fetchEntityInfo(req.EntityID)
		if err != nil {
			logger.Warn("entity_info", "err", err)
			publishRecordingState(req.ClientID, recordingStateMsg{
				Room: req.Room, Status: "error", Target: req.Target,
				Error: "authorization unavailable",
			})
			return
		}
		if info.EntityID == "" || !info.IsAdmin {
			logger.Warn("recording.start denied (not admin)", "entity", req.EntityID)
			publishRecordingState(req.ClientID, recordingStateMsg{
				Room: req.Room, Status: "error", Target: req.Target,
				Error: "admin only",
			})
			audit.Emit(nc, "recording.start_denied", audit.SeverityWarn,
				audit.Actor{EntityID: req.EntityID, ClientID: req.ClientID, Extension: extID},
				audit.Details{"room": req.Room, "target": req.Target, "reason": "not_admin"},
				"")
			return
		}

		mu.Lock()
		if _, exists := activeRecs[req.Room]; exists {
			mu.Unlock()
			publishRecordingState(req.ClientID, recordingStateMsg{
				Room: req.Room, Status: "error", Target: req.Target,
				Error: "room already recording",
			})
			return
		}
		mu.Unlock()

		// TODO(step 5): start Room Composite Egress, insert PB row, audit emit,
		// publish recording_state (active) + recording_active to participants.
		logger.Info("recording.start authorized (start not yet implemented)",
			"entity", req.EntityID, "room", req.Room, "target", req.Target)
		_ = activeRecs
	})

	// --- recording.stop handler (skeleton: validate + reject; stop wired in step 5) ---
	nc.Subscribe("recording.stop", func(m *nats.Msg) {
		var req recordingStopMsg
		if err := json.Unmarshal(m.Data, &req); err != nil {
			logger.Warn("recording.stop unmarshal", "err", err)
			return
		}
		if req.Room == "" || req.ClientID == "" || req.EntityID == "" {
			logger.Warn("recording.stop missing fields", "req", req)
			return
		}
		// Stop is admin-only too: only the host that started (or another admin)
		// can stop. We authorize via entity_info for consistency.
		info, err := fetchEntityInfo(req.EntityID)
		if err != nil {
			logger.Warn("entity_info", "err", err)
			publishRecordingState(req.ClientID, recordingStateMsg{
				Room: req.Room, Status: "error", Error: "authorization unavailable",
			})
			return
		}
		if info.EntityID == "" || !info.IsAdmin {
			logger.Warn("recording.stop denied (not admin)", "entity", req.EntityID)
			publishRecordingState(req.ClientID, recordingStateMsg{
				Room: req.Room, Status: "error", Error: "admin only",
			})
			return
		}

		mu.Lock()
		rec, exists := activeRecs[req.Room]
		mu.Unlock()
		if !exists {
			publishRecordingState(req.ClientID, recordingStateMsg{
				Room: req.Room, Status: "error", Error: "no active recording for room",
			})
			return
		}

		// TODO(step 5): StopEgress, update PB row, audit emit, publish state/active.
		logger.Info("recording.stop authorized (stop not yet implemented)",
			"entity", req.EntityID, "room", req.Room, "egress", rec.EgressID)
	})

	// --- Extension registration protocol ---
	regSubject := fmt.Sprintf("extension.%s.register", extID)
	hbSubject := fmt.Sprintf("extension.%s.heartbeat", extID)

	publishReg := func() {
		regData, _ := json.Marshal(registerMsg{
			ExtensionID:        extID,
			HeartbeatIntervalS: heartbeatS,
			OptionsSchema:      nil, // no hot-reloadable options in v1
		})
		nc.Publish(regSubject, regData)
		nc.Publish(hbSubject, []byte(extID))
	}

	readyCh := make(chan struct{}, 1)
	nc.Subscribe("worldsim.ready", func(m *nats.Msg) {
		logger.Info("worldsim ready", "map", string(m.Data))
		publishReg()
		select {
		case readyCh <- struct{}{}:
		default:
		}
	})

	// Wait for worldsim.ready before initial registration.
	select {
	case <-readyCh:
	case <-time.After(10 * time.Second):
		logger.Warn("worldsim.ready not received, registering anyway", "id", extID)
		publishReg()
	}

	// Heartbeat + re-register loop.
	ticker := time.NewTicker(time.Duration(heartbeatS) * time.Second)
	defer ticker.Stop()

	var ticks int
	for {
		select {
		case <-ctx.Done():
			logger.Info("shutting down")
			return
		case <-ticker.C:
			nc.Publish(hbSubject, []byte(extID))
			if ticks%3 == 0 {
				publishReg()
			}
			publishHealth(nc, "ext-"+extID, startTime)
			ticks++
		}
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func publishHealth(nc *nats.Conn, service string, startTime time.Time) {
	health := map[string]any{
		"service": service,
		"status":  "OK",
		"version": version.Version,
		"uptime":  time.Since(startTime).Round(time.Second).String(),
		"extras":  map[string]any{},
	}
	data, _ := json.Marshal(health)
	nc.Publish("healthz", data)
}
