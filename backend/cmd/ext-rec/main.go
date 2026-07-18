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
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
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
	ClientID    string `json:"client_id,omitempty"`
}

// recordingCreateMsg is the request payload for worldsim.recording.create.
type recordingCreateMsg struct {
	MeetingID    string   `json:"meeting_id"`
	Room         string   `json:"room"`
	ZoneID       string   `json:"zone_id,omitempty"`
	MapID        string   `json:"map_id,omitempty"`
	Target       string   `json:"target"`
	EgressID     string   `json:"egress_id"`
	StartedBy    string   `json:"started_by"`
	Participants []string `json:"participants,omitempty"`
	StartTime    int64    `json:"start_time"`
	Status       string   `json:"status"`
	FileURL      string   `json:"file_url,omitempty"`
	AudioURL     string   `json:"audio_url,omitempty"`
}

type recordingCreateReply struct {
	OK       bool   `json:"ok"`
	RecordID string `json:"record_id,omitempty"`
	Error    string `json:"error,omitempty"`
}

// recordingUpdateMsg is the request payload for worldsim.recording.update.
type recordingUpdateMsg struct {
	MeetingID   string `json:"meeting_id"`
	EndTime     int64  `json:"end_time,omitempty"`
	Status      string `json:"status,omitempty"`
	FileURL     string `json:"file_url,omitempty"`
	AudioURL    string `json:"audio_url,omitempty"`
	AudioStatus string `json:"audio_status,omitempty"`
	AudioError  string `json:"audio_error,omitempty"`
}

type recordingUpdateReply struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
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
	Filename     string // MP4 filename (for audio extraction + file_url)
}

func main() {
	startTime := time.Now()
	natsURL := envOr("NATS_URL", "nats://localhost:4222")
	extID := envOr("EXTENSION_ID", "rec")
	livekitURL := envOr("LIVEKIT_URL", "ws://localhost:7880")
	livekitAPIKey := os.Getenv("LIVEKIT_API_KEY")
	livekitAPISecret := os.Getenv("LIVEKIT_API_SECRET")
	recordingsDir := envOr("RECORDINGS_DIR", "./recordings")
	publicHost := envOr("PUBLIC_HOST", "localhost")
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
	// emptySince tracks when a room's participant count last dropped to
	// zero while a recording is active. Used by the auto-stop ticker.
	emptySince := make(map[string]time.Time)

	// LiveKit API clients. The egress client starts/stops recordings; the
	// room client lists participants (for the participant snapshot and the
	// recording_active fan-out).
	egressClient := lksdk.NewEgressClient(livekitURL, livekitAPIKey, livekitAPISecret)
	roomClient := lksdk.NewRoomServiceClient(livekitURL, livekitAPIKey, livekitAPISecret)

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

	// createRecordingRow calls worldsim.recording.create to insert a PB row.
	createRecordingRow := func(msg recordingCreateMsg) error {
		data, _ := json.Marshal(msg)
		reply, err := nc.Request("worldsim.recording.create", data, 3*time.Second)
		if err != nil {
			return fmt.Errorf("recording.create request: %w", err)
		}
		var resp recordingCreateReply
		if err := json.Unmarshal(reply.Data, &resp); err != nil {
			return fmt.Errorf("recording.create unmarshal: %w", err)
		}
		if !resp.OK {
			return fmt.Errorf("recording.create: %s", resp.Error)
		}
		return nil
	}

	// updateRecordingRow calls worldsim.recording.update to update a PB row.
	updateRecordingRow := func(msg recordingUpdateMsg) error {
		data, _ := json.Marshal(msg)
		reply, err := nc.Request("worldsim.recording.update", data, 3*time.Second)
		if err != nil {
			return fmt.Errorf("recording.update request: %w", err)
		}
		var resp recordingUpdateReply
		if err := json.Unmarshal(reply.Data, &resp); err != nil {
			return fmt.Errorf("recording.update unmarshal: %w", err)
		}
		if !resp.OK {
			return fmt.Errorf("recording.update: %s", resp.Error)
		}
		return nil
	}

	// listRoomParticipants returns the entity_ids (LiveKit participant
	// identities) currently in the room. Used for the participant snapshot
	// and the recording_active fan-out.
	listRoomParticipants := func(roomName string) ([]string, error) {
		resp, err := roomClient.ListParticipants(ctx, &livekit.ListParticipantsRequest{Room: roomName})
		if err != nil {
			return nil, fmt.Errorf("list participants: %w", err)
		}
		ids := make([]string, 0, len(resp.Participants))
		for _, p := range resp.Participants {
			ids = append(ids, p.Identity)
		}
		return ids, nil
	}

	// buildEgressRequest constructs a RoomCompositeEgressRequest for the
	// given target. MP4 writes to RECORDINGS_DIR; YouTube streams RTMP.
	// Returns the request and the filename (for MP4) used to build file_url.
	buildEgressRequest := func(room, target string) (*livekit.RoomCompositeEgressRequest, string, error) {
		req := &livekit.RoomCompositeEgressRequest{
			RoomName: room,
			Layout:   "speaker",
		}
		filename := ""
		switch target {
		case "mp4":
			filename = fmt.Sprintf("%s-%d.mp4", room, time.Now().Unix())
			req.FileOutputs = []*livekit.EncodedFileOutput{{
				FileType: livekit.EncodedFileType_MP4,
				Filepath: filepath.Join(recordingsDir, filename),
			}}
		case "youtube":
			if youtubeRTMPURL == "" || youtubeStreamKey == "" {
				return nil, "", fmt.Errorf("YOUTUBE_RTMP_URL and YOUTUBE_STREAM_KEY must be set for youtube target")
			}
			rtmpURL := strings.TrimRight(youtubeRTMPURL, "/") + "/" + youtubeStreamKey
			req.StreamOutputs = []*livekit.StreamOutput{{
				Protocol: livekit.StreamProtocol_RTMP,
				Urls:     []string{rtmpURL},
			}}
		default:
			return nil, "", fmt.Errorf("invalid target %q", target)
		}
		return req, filename, nil
	}

	// fanOutRecordingActive publishes recording_active to all participants in
	// the room. Each participant's client_id is resolved via entity_info.
	fanOutRecordingActive := func(room, target string, active bool, participants []string) {
		msg := recordingActiveMsg{Room: room, Active: active, Target: target}
		for _, entityID := range participants {
			info, err := fetchEntityInfo(entityID)
			if err != nil || info.ClientID == "" {
				logger.Warn("fanOut: could not resolve client_id", "entity", entityID, "err", err)
				continue
			}
			publishRecordingActive(info.ClientID, msg)
		}
	}

	// audioSem caps concurrent ffmpeg extractions so a burst of stop
	// events doesn't saturate CPU. Capacity 2 for now; make it an env
	// var later if tunability is needed.
	audioSem := make(chan struct{}, 2)

	// extractAudioAndUpdatePB polls for the MP4 file to appear on disk
	// (Egress writes it asynchronously after StopEgress), then runs ffmpeg
	// to extract the audio track as MP3, and updates the PB row with the
	// audio_url + audio_status. On failure, sets audio_status="failed"
	// with audio_error and emits a recording.audio_extraction_failed audit
	// event (SeverityError) so it shows up on the audit dashboard.
	extractAudioAndUpdatePB := func(meetingID, room, mp4Filename string) {
		audioSem <- struct{}{}
		defer func() { <-audioSem }()

		fail := func(reason, errMsg string) {
			logger.Warn("audio extraction failed", "meeting", meetingID, "reason", reason, "err", errMsg)
			if err := updateRecordingRow(recordingUpdateMsg{
				MeetingID:   meetingID,
				AudioStatus: "failed",
				AudioError:  reason,
			}); err != nil {
				logger.Warn("audio extraction: update PB row (failed)", "err", err, "meeting", meetingID)
			}
			audit.Emit(nc, "recording.audio_extraction_failed", audit.SeverityError,
				audit.Actor{Extension: extID},
				audit.Details{
					"meeting_id": meetingID,
					"room":       room,
					"file":       mp4Filename,
					"reason":     reason,
					"error":      errMsg,
				},
				"")
		}

		mp4Path := filepath.Join(recordingsDir, mp4Filename)
		// Poll up to 60s for the MP4 to appear and stop growing.
		var lastSize int64
		stableCount := 0
		deadline := time.Now().Add(60 * time.Second)
		for time.Now().Before(deadline) {
			info, err := os.Stat(mp4Path)
			if err == nil {
				if info.Size() == lastSize && info.Size() > 0 {
					stableCount++
					if stableCount >= 2 {
						break
					}
				} else {
					stableCount = 0
				}
				lastSize = info.Size()
			}
			time.Sleep(2 * time.Second)
		}
		if _, err := os.Stat(mp4Path); err != nil {
			fail("mp4 not found after 60s", err.Error())
			return
		}

		// Derive MP3 filename from the MP4 filename.
		mp3Filename := strings.TrimSuffix(mp4Filename, ".mp4") + ".mp3"
		mp3Path := filepath.Join(recordingsDir, mp3Filename)

		// ffmpeg -i input.mp4 -vn -acodec libmp3lame -q:a 2 output.mp3
		// 10min timeout: libmp3lame at q:a 2 is roughly real-time, so a
		// 1h meeting extracts in ~1-2min. 10min covers long meetings
		// with headroom; beyond that something is wrong (hung ffmpeg,
		// disk full, etc.) and we'd rather fail and emit an audit event
		// than hold a semaphore slot forever.
		ffmpegCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ffmpegCtx, "ffmpeg",
			"-y",
			"-i", mp4Path,
			"-vn",
			"-acodec", "libmp3lame",
			"-q:a", "2",
			mp3Path,
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			reason := "ffmpeg failed"
			if ffmpegCtx.Err() == context.DeadlineExceeded {
				reason = "ffmpeg timed out after 10m"
			}
			fail(reason, string(out))
			return
		}

		audioURL := fmt.Sprintf("https://%s/recordings/%s", publicHost, mp3Filename)
		if err := updateRecordingRow(recordingUpdateMsg{
			MeetingID:   meetingID,
			AudioURL:    audioURL,
			AudioStatus: "ok",
			AudioError:  "",
		}); err != nil {
			// ffmpeg succeeded but PB update failed — the MP3 exists on
			// disk; surface the PB error so an admin can re-trigger.
			fail("pb update after ffmpeg success", err.Error())
			return
		}
		logger.Info("audio extracted", "meeting", meetingID, "file", mp3Filename)
	}

	// --- recording.start handler ---
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

		// Build and start the Egress.
		egressReq, filename, err := buildEgressRequest(req.Room, req.Target)
		if err != nil {
			logger.Warn("build egress request", "err", err)
			publishRecordingState(req.ClientID, recordingStateMsg{
				Room: req.Room, Status: "error", Target: req.Target, Error: err.Error(),
			})
			return
		}
		egressInfo, err := egressClient.StartRoomCompositeEgress(ctx, egressReq)
		if err != nil {
			logger.Warn("start egress", "err", err, "room", req.Room)
			publishRecordingState(req.ClientID, recordingStateMsg{
				Room: req.Room, Status: "error", Target: req.Target, Error: err.Error(),
			})
			return
		}

		// Snapshot participants for the PB row + recording_active fan-out.
		participants, _ := listRoomParticipants(req.Room)

		// Build the download URL for MP4 target. YouTube has no file URL.
		fileURL := ""
		if filename != "" {
			fileURL = fmt.Sprintf("https://%s/recordings/%s", publicHost, filename)
		}

		meetingID := uuid.NewString()
		rec := &activeRec{
			EgressID:     egressInfo.EgressId,
			Room:         req.Room,
			Target:       req.Target,
			StartedBy:    req.EntityID,
			StartedAt:    time.Now(),
			Participants: participants,
			MeetingID:    meetingID,
			Filename:     filename,
		}

		mu.Lock()
		activeRecs[req.Room] = rec
		mu.Unlock()

		// Insert PB row via worldsim.
		zoneID := ""
		if strings.HasPrefix(req.Room, "zone-") {
			zoneID = strings.TrimPrefix(req.Room, "zone-")
		}
		createErr := createRecordingRow(recordingCreateMsg{
			MeetingID:    meetingID,
			Room:         req.Room,
			ZoneID:       zoneID,
			MapID:        info.MapID,
			Target:       req.Target,
			EgressID:     egressInfo.EgressId,
			StartedBy:    req.EntityID,
			Participants: participants,
			StartTime:    rec.StartedAt.UnixMilli(),
			Status:       "active",
			FileURL:      fileURL,
		})
		if createErr != nil {
			logger.Warn("create recording row", "err", createErr, "meeting", meetingID)
			// Non-fatal: the Egress is running; we just couldn't persist metadata.
		}

		audit.Emit(nc, "recording.start", audit.SeverityInfo,
			audit.Actor{EntityID: req.EntityID, ClientID: req.ClientID, Extension: extID},
			audit.Details{
				"room": req.Room, "target": req.Target, "egress_id": egressInfo.EgressId,
				"meeting_id": meetingID, "participants": participants,
			},
			"")

		publishRecordingState(req.ClientID, recordingStateMsg{
			Room: req.Room, Status: "active", Target: req.Target, EgressID: egressInfo.EgressId,
		})
		fanOutRecordingActive(req.Room, req.Target, true, participants)
		logger.Info("recording started",
			"entity", req.EntityID, "room", req.Room, "target", req.Target,
			"egress", egressInfo.EgressId, "meeting", meetingID, "participants", len(participants))
	})

	// stopRecording performs the full stop flow: StopEgress, remove from
	// activeRecs, update PB row to completed, emit audit event, fan out
	// recording_active=false, and kick off audio extraction. Called from
	// the manual recording.stop handler and the auto-stop-on-empty ticker.
	// reason is "manual" or "auto_empty"; actor is the audit actor (manual
	// has entity/client, auto has only extension).
	stopRecording := func(room string, rec *activeRec, reason string, actor audit.Actor) {
		_, err := egressClient.StopEgress(ctx, &livekit.StopEgressRequest{EgressId: rec.EgressID})
		if err != nil {
			logger.Warn("stop egress", "err", err, "egress", rec.EgressID)
			// Continue to clean up state even if StopEgress fails — the Egress
			// may have already ended (room empty, server restart, etc.).
		}

		mu.Lock()
		delete(activeRecs, room)
		delete(emptySince, room)
		mu.Unlock()

		// Update PB row.
		updateErr := updateRecordingRow(recordingUpdateMsg{
			MeetingID: rec.MeetingID,
			EndTime:   time.Now().UnixMilli(),
			Status:    "completed",
		})
		if updateErr != nil {
			logger.Warn("update recording row", "err", updateErr, "meeting", rec.MeetingID)
		}

		audit.Emit(nc, "recording.stop", audit.SeverityInfo,
			actor,
			audit.Details{
				"room": room, "target": rec.Target, "egress_id": rec.EgressID,
				"meeting_id": rec.MeetingID, "reason": reason,
			},
			"")

		// Notify the host if we have a client_id (manual stop). Auto-stop
		// has no client_id — the host has already disconnected — so we
		// skip the per-client state push and just fan out active=false.
		if actor.ClientID != "" {
			publishRecordingState(actor.ClientID, recordingStateMsg{
				Room: room, Status: "stopped", Target: rec.Target, EgressID: rec.EgressID,
			})
		}
		fanOutRecordingActive(room, rec.Target, false, rec.Participants)
		logger.Info("recording stopped",
			"reason", reason, "entity", actor.EntityID, "room", room, "egress", rec.EgressID, "meeting", rec.MeetingID)

		// Extract audio (MP3) from the MP4 in the background. The Egress
		// writes the file asynchronously after StopEgress returns, so we
		// poll for it. On success, update the PB row with audio_url.
		// audio_status is set to "pending" immediately so the UI can show
		// "extracting..." while the goroutine runs.
		if rec.Target == "mp4" && rec.Filename != "" {
			if err := updateRecordingRow(recordingUpdateMsg{
				MeetingID:   rec.MeetingID,
				AudioStatus: "pending",
			}); err != nil {
				logger.Warn("audio extraction: set pending status", "err", err, "meeting", rec.MeetingID)
			}
			go extractAudioAndUpdatePB(rec.MeetingID, rec.Room, rec.Filename)
		}
	}

	// --- recording.stop handler ---
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

		stopRecording(req.Room, rec, "manual",
			audit.Actor{EntityID: req.EntityID, ClientID: req.ClientID, Extension: extID})
	})

	// --- recording.admin.stop handler ---
	// Triggered by the admin UI "Stop" button. Bypasses the entity/admin
	// check (the admin server already authenticated the user via session
	// cookie) and uses the admin email as the audit actor identity.
	nc.Subscribe("recording.admin.stop", func(m *nats.Msg) {
		var req struct {
			Room       string `json:"room"`
			MeetingID  string `json:"meeting_id"`
			AdminEmail string `json:"admin_email"`
		}
		if err := json.Unmarshal(m.Data, &req); err != nil {
			logger.Warn("recording.admin.stop unmarshal", "err", err)
			return
		}
		if req.Room == "" || req.MeetingID == "" {
			logger.Warn("recording.admin.stop missing fields", "req", req)
			return
		}
		mu.Lock()
		rec, exists := activeRecs[req.Room]
		mu.Unlock()
		if !exists {
			logger.Warn("recording.admin.stop: no active recording for room",
				"room", req.Room, "meeting", req.MeetingID)
			return
		}
		if rec.MeetingID != req.MeetingID {
			logger.Warn("recording.admin.stop: meeting_id mismatch",
				"room", req.Room, "want", req.MeetingID, "got", rec.MeetingID)
			return
		}
		stopRecording(req.Room, rec, "admin_stop",
			audit.Actor{EntityID: req.AdminEmail, Extension: extID})
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

	// Auto-stop ticker: every 1s, check each active recording's room for
	// participants. If a room has zero participants, mark the time; if it
	// stays empty for 10 consecutive seconds, stop the recording with
	// reason="auto_empty". Any participant rejoining resets the timer.
	autoStopTicker := time.NewTicker(1 * time.Second)
	defer autoStopTicker.Stop()
	const emptyTimeout = 10 * time.Second
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-autoStopTicker.C:
				// Snapshot active recordings under the lock.
				mu.Lock()
				type pending struct {
					room string
					rec  *activeRec
				}
				var toCheck []pending
				for room, rec := range activeRecs {
					toCheck = append(toCheck, pending{room, rec})
				}
				mu.Unlock()

				for _, p := range toCheck {
					participants, err := listRoomParticipants(p.room)
					if err != nil {
						// LiveKit unreachable or room gone — leave the timer
						// alone; next tick will retry. Don't auto-stop on
						// transient errors.
						continue
					}
					mu.Lock()
					if len(participants) > 0 {
						delete(emptySince, p.room)
						mu.Unlock()
						continue
					}
					// Room is empty.
					since, ok := emptySince[p.room]
					if !ok {
						emptySince[p.room] = time.Now()
						mu.Unlock()
						continue
					}
					elapsed := time.Since(since)
					mu.Unlock()
					if elapsed >= emptyTimeout {
						// Re-fetch rec under lock to make sure it's still
						// active and hasn't been stopped concurrently.
						mu.Lock()
						rec, exists := activeRecs[p.room]
						mu.Unlock()
						if !exists {
							continue
						}
						logger.Info("auto-stop: room empty for >=10s", "room", p.room, "meeting", rec.MeetingID)
						stopRecording(p.room, rec, "auto_empty",
							audit.Actor{Extension: extID})
					}
				}
			}
		}
	}()

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
