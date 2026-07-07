// Fetches the character spritesheet catalog from PocketBase.
//
// The `sprite_bases` collection (see pb_migrations/1751900000_create_sprite_bases.js)
// stores each base sheet as a file field. This module fetches all records and
// returns the IDs + URLs so GameScene can load them via Phaser's loader.
//
// Env vars:
//   VITE_POCKETBASE_URL — PocketBase base URL (dev only; default http://localhost:8090)
//
// In production (served by nginx), PB_URL derives from window.location.origin so
// the browser reaches PocketBase via the nginx /api/ proxy (same-origin).

const PB_URL = window.location.port === "5173"
  ? (import.meta.env.VITE_POCKETBASE_URL ?? "http://localhost:8090")
  : window.location.origin;

export interface SpriteBaseAsset {
  // PocketBase record ID — used as the Phaser texture key.
  id: string;
  // Human-readable name (filename stem, e.g. "char_0").
  name: string;
  // PocketBase file URL for the PNG.
  url: string;
}

// Fetch all sprite_bases records from PocketBase. Returns empty array on
// failure (caller falls back to static char_0..char_4 files).
export async function loadSpriteBases(): Promise<SpriteBaseAsset[]> {
  try {
    const resp = await fetch(
      `${PB_URL}/api/collections/sprite_bases/records?perPage=200`,
    );
    if (!resp.ok) throw new Error(`PocketBase responded ${resp.status}`);

    const data = await resp.json();
    if (!data.items || data.items.length === 0) {
      return [];
    }

    return data.items.map((item: { id: string; name: string; sheet: string; collectionId: string }) => ({
      id: item.id,
      name: item.name,
      url: `${PB_URL}/api/files/${item.collectionId}/${item.id}/${item.sheet}`,
    }));
  } catch (err) {
    console.warn("PocketBase sprite_bases load failed, falling back to static sheets:", err);
    return [];
  }
}
