// WorldOptions client — fetches the server-wide runtime config from the
// worldsim HTTP endpoint GET /api/world-options (admin-gated via the users
// JWT). Used by the "Stream to YouTube" confirm modal to pre-fill the RTMP
// URL and stream key from the admin-configured defaults.
//
// The endpoint returns only the YouTube fields + public_host (not the full
// options) to keep SMTP passwords and other sensitive fields server-side.
// Non-admin users get a 403; the modal is only shown to admins anyway.

import { pb } from "../auth";

export interface WorldOptionsYouTube {
  youtube_rtmp_url: string;
  youtube_stream_key: string;
  public_host: string;
  recording_enabled: boolean;
}

let cached: WorldOptionsYouTube | null = null;
let inflight: Promise<WorldOptionsYouTube | null> | null = null;

// fetchWorldOptions returns the admin-gated world_options subset. Returns
// null on any error (non-admin, network failure, etc.) so the caller can
// fall back to empty defaults. Cached after the first successful fetch;
// call refreshWorldOptions to re-fetch after an admin edits the options.
export async function fetchWorldOptions(): Promise<WorldOptionsYouTube | null> {
  if (cached) return cached;
  if (inflight) return inflight;
  inflight = (async () => {
    try {
      const res = await pb.send("/world-options", { method: "GET" });
      cached = {
        youtube_rtmp_url: res.youtube_rtmp_url ?? "",
        youtube_stream_key: res.youtube_stream_key ?? "",
        public_host: res.public_host ?? "",
        recording_enabled: res.recording_enabled !== false,
      };
      return cached;
    } catch {
      return null;
    } finally {
      inflight = null;
    }
  })();
  return inflight;
}

export function clearWorldOptionsCache(): void {
  cached = null;
}
