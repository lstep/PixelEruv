# Roadmap (future features)

## AI meeting transcription

Plug an AI service to read a recording's MP4 and produce a transcript, displayed alongside the video on the recordings management page (`/admin/recordings`).

### Sketch

- Background worker picks up completed recordings (status=`completed`, no transcript yet).
- Worker calls a speech-to-text API (e.g. OpenAI Whisper) on the MP4 file from the `recordings` volume.
- Transcript stored in a new `transcript` field on the `recordings` PocketBase collection (JSON: segments with start/end timestamps + text).
- Recordings page renders the transcript next to the `<video>` element in the modal player, with click-to-seek on each segment.
- Transcript is downloadable (plain text + JSON + SRT formats) from the recordings page.
- Re-transcribe button on the recordings row (admin only) for when the model improves or the original run failed.

### Open questions

- Hosted API (Whisper, Deepgram) vs. self-hosted (faster-whisper on GPU)?
- Speaker diarization (who said what) — requires participant audio tracks, not just the room composite mix. May need a separate Egress with per-track outputs.
- Cost/latency budget for long meetings.
- Language detection vs. per-meeting language selection.
