package worldsim

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/types"
)

// RecordingStore handles PocketBase recordings collection writes via the
// in-process Go SDK (DAO access, no HTTP). Mirrors the BanStore pattern.
// ext-rec calls it via the worldsim.recording.create / .update NATS
// request-reply subjects — extensions don't have PocketBase access.
type RecordingStore struct {
	app core.App
}

func NewRecordingStore(app core.App) *RecordingStore {
	return &RecordingStore{app: app}
}

// recordingCreateMsg is the request payload for worldsim.recording.create.
type recordingCreateMsg struct {
	MeetingID    string   `json:"meeting_id"`
	Room         string   `json:"room"`
	ZoneID       string   `json:"zone_id,omitempty"`
	MapID        string   `json:"map_id,omitempty"`
	Target       string   `json:"target"` // "mp4" | "youtube"
	EgressID     string   `json:"egress_id"`
	StartedBy    string   `json:"started_by"`
	Participants []string `json:"participants,omitempty"`
	StartTime    int64    `json:"start_time"` // unix millis
	Status       string   `json:"status"`     // "active"
	FileURL      string   `json:"file_url,omitempty"`
	AudioURL     string   `json:"audio_url,omitempty"`
}

// recordingCreateReply is the reply for worldsim.recording.create.
type recordingCreateReply struct {
	OK      bool   `json:"ok"`
	RecordID string `json:"record_id,omitempty"`
	Error   string `json:"error,omitempty"`
}

// Create inserts a new row in the recordings collection.
func (s *RecordingStore) Create(msg recordingCreateMsg) (string, error) {
	collection, err := s.app.FindCollectionByNameOrId("recordings")
	if err != nil {
		return "", fmt.Errorf("find recordings collection: %w", err)
	}
	record := core.NewRecord(collection)
	record.Set("meeting_id", msg.MeetingID)
	record.Set("room", msg.Room)
	record.Set("zone_id", msg.ZoneID)
	record.Set("map_id", msg.MapID)
	record.Set("target", msg.Target)
	record.Set("egress_id", msg.EgressID)
	record.Set("started_by", msg.StartedBy)
	if len(msg.Participants) > 0 {
		participantsJSON, _ := json.Marshal(msg.Participants)
		record.Set("participants", json.RawMessage(participantsJSON))
	}
	record.Set("start_time", types.NowDateTime())
	record.Set("status", msg.Status)
	record.Set("file_url", msg.FileURL)
	record.Set("audio_url", msg.AudioURL)
	// consent_state: record who was notified (same as participants at start).
	if len(msg.Participants) > 0 {
		consent := map[string]any{
			"notified_participants": msg.Participants,
			"consented":             []string{},
		}
		consentJSON, _ := json.Marshal(consent)
		record.Set("consent_state", json.RawMessage(consentJSON))
	}
	if err := s.app.Save(record); err != nil {
		return "", fmt.Errorf("save recording: %w", err)
	}
	return record.Id, nil
}

// recordingUpdateMsg is the request payload for worldsim.recording.update.
type recordingUpdateMsg struct {
	MeetingID string `json:"meeting_id"` // identifies the row to update
	EndTime   int64  `json:"end_time,omitempty"`
	Status    string `json:"status,omitempty"`
	FileURL   string `json:"file_url,omitempty"`
	AudioURL  string `json:"audio_url,omitempty"`
}

// recordingUpdateReply is the reply for worldsim.recording.update.
type recordingUpdateReply struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// Update finds a recording by meeting_id and updates the provided fields.
func (s *RecordingStore) Update(msg recordingUpdateMsg) error {
	record, err := s.app.FindFirstRecordByFilter(
		"recordings",
		"meeting_id = {:mid}",
		map[string]any{"mid": msg.MeetingID},
	)
	if err != nil {
		return fmt.Errorf("find recording %s: %w", msg.MeetingID, err)
	}
	if record == nil {
		return fmt.Errorf("recording %s not found", msg.MeetingID)
	}
	if msg.EndTime > 0 {
		endTime, err := types.ParseDateTime(time.UnixMilli(msg.EndTime))
		if err == nil {
			record.Set("end_time", endTime)
		}
	}
	if msg.Status != "" {
		record.Set("status", msg.Status)
	}
	if msg.FileURL != "" {
		record.Set("file_url", msg.FileURL)
	}
	if msg.AudioURL != "" {
		record.Set("audio_url", msg.AudioURL)
	}
	return s.app.Save(record)
}

// subscribeRecordingStore sets up the worldsim.recording.create and
// worldsim.recording.update request-reply handlers so ext-rec can persist
// recording metadata without direct PocketBase access.
func (s *Simulator) subscribeRecordingStore() error {
	if _, err := s.nc.Subscribe("worldsim.recording.create", func(msg *nats.Msg) {
		var req recordingCreateMsg
		if err := json.Unmarshal(msg.Data, &req); err != nil {
			s.logger.Warn("recording.create unmarshal", "err", err)
			reply, _ := json.Marshal(recordingCreateReply{Error: "unmarshal"})
			msg.Respond(reply)
			return
		}
		id, err := s.recordingStore.Create(req)
		if err != nil {
			s.logger.Warn("recording.create", "err", err, "meeting", req.MeetingID)
			reply, _ := json.Marshal(recordingCreateReply{Error: err.Error()})
			msg.Respond(reply)
			return
		}
		reply, _ := json.Marshal(recordingCreateReply{OK: true, RecordID: id})
		msg.Respond(reply)
	}); err != nil {
		return err
	}
	if _, err := s.nc.Subscribe("worldsim.recording.update", func(msg *nats.Msg) {
		var req recordingUpdateMsg
		if err := json.Unmarshal(msg.Data, &req); err != nil {
			reply, _ := json.Marshal(recordingUpdateReply{Error: "unmarshal"})
			msg.Respond(reply)
			return
		}
		if err := s.recordingStore.Update(req); err != nil {
			s.logger.Warn("recording.update", "err", err, "meeting", req.MeetingID)
			reply, _ := json.Marshal(recordingUpdateReply{Error: err.Error()})
			msg.Respond(reply)
			return
		}
		reply, _ := json.Marshal(recordingUpdateReply{OK: true})
		msg.Respond(reply)
	}); err != nil {
		return err
	}
	return nil
}
