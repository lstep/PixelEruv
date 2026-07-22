// Fetches the character spritesheet catalog from worldsim's asset API.
//
// worldsim serves the sprite_bases list via GET /api/assets/sprites. The
// sprite_bases PB collection is fully locked (nil API rules), so the frontend
// cannot hit /api/collections/sprite_bases/ directly. worldsim reads the
// records via the in-process Go SDK (bypassing rules) and serves the list +
// sheet file URLs on its embedded PB router.
//
// Env vars:
//   VITE_POCKETBASE_URL — worldsim/PocketBase base URL (dev only; default http://localhost:8090)
//
// In production (served by nginx), PB_URL derives from window.location.origin so
// the browser reaches worldsim via the nginx /api/ proxy (same-origin).

const PB_URL = window.location.port === "5173"
  ? (import.meta.env.VITE_POCKETBASE_URL ?? "http://localhost:8090")
  : window.location.origin;

export interface SpriteBaseAsset {
  // PocketBase record ID — used as the Phaser texture key.
  id: string;
  // Human-readable name (filename stem, e.g. "char_0").
  name: string;
  // URL to the sprite PNG (served by worldsim's asset endpoint).
  url: string;
}

// Response shape from GET /api/assets/sprites.
interface SpriteBaseResponse {
  id: string;
  name: string;
  url: string;
}

// Fetch all sprite_bases from worldsim. Returns empty array on failure (caller
// falls back to static char_0..char_3 files).
export async function loadSpriteBases(): Promise<SpriteBaseAsset[]> {
  try {
    const resp = await fetch(`${PB_URL}/api/assets/sprites`);
    if (!resp.ok) throw new Error(`Asset endpoint responded ${resp.status}`);

    const data: SpriteBaseResponse[] = await resp.json();
    if (!data || data.length === 0) {
      return [];
    }

    return data.map((item) => ({
      id: item.id,
      name: item.name,
      url: item.url.startsWith("http") ? item.url : `${PB_URL}${item.url}`,
    }));
  } catch (err) {
    console.warn("Sprite bases load failed, falling back to static sheets:", err);
    return [];
  }
}
