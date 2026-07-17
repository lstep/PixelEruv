// Fetches map assets (Tiled JSON + tileset images) from PocketBase.
//
// The `maps` collection (see pb_migrations/) stores the Tiled JSON export and
// tileset images as file fields. This module fetches the record, retrieves the
// JSON to extract tileset names, and returns everything GameScene needs to
// load the map via Phaser's loader.
//
// When no map name is given, the map marked `is_default=true` in PocketBase is
// loaded (this is where new players spawn). If no map has `is_default=true`,
// falls back to querying by name "main" for backwards compatibility.
//
// Env vars:
//   VITE_POCKETBASE_URL — PocketBase base URL (dev only; default http://localhost:8090)
//
// In production (served by nginx), PB_URL derives from window.location.origin so
// the browser reaches PocketBase via the nginx /api/ proxy (same-origin), which
// works for both localhost and remote access without hardcoded addresses.

const PB_URL = window.location.port === "5173"
  ? (import.meta.env.VITE_POCKETBASE_URL ?? "http://localhost:8090")
  : window.location.origin;

export interface TilesetAsset {
  // The tileset name as defined in the Tiled JSON (used by addTilesetImage).
  name: string;
  // PocketBase file URL for the tileset image.
  url: string;
}

export interface MapAssets {
  // The PB record name of the loaded map. Used to detect when the server
  // wants the player on a different map than the one initially loaded.
  name: string;
  // Parsed Tiled JSON object — passed directly to load.tilemapTiledJSON.
  tiledJson: object;
  // One entry per tileset in the map.
  tilesets: TilesetAsset[];
}

// Minimal shape of a Tiled map JSON — only the fields we read.
interface TiledMap {
  tilesets: { name: string; image: string }[];
}

// Minimal shape of a PB maps record — only the fields we read.
interface MapRecord {
  id: string;
  collectionId: string;
  name: string;
  tiled_json: string;
  tilesets: string[];
}

// Fetch the configured map from PocketBase. Throws if PB is unreachable or the
// map record doesn't exist — the caller should fall back to static files.
// If mapName is omitted, loads the map marked is_default=true (falling back to
// name "main" if no map is marked default).
export async function loadMapAssets(mapName?: string): Promise<MapAssets> {
  const record = mapName
    ? await fetchRecordByName(mapName)
    : await fetchDefaultRecord();

  const fileBase = `${PB_URL}/api/files/${record.collectionId}/${record.id}`;

  // Fetch the Tiled JSON so we can extract tileset names and pass the parsed
  // object to Phaser (avoids a double fetch by the loader).
  const tiledJsonUrl = `${fileBase}/${record.tiled_json}`;
  const jsonResp = await fetch(tiledJsonUrl);
  if (!jsonResp.ok) throw new Error(`Failed to fetch Tiled JSON: ${jsonResp.status}`);
  const tiledJson: object = await jsonResp.json();
  const tiled = tiledJson as TiledMap;

  // Build tileset URLs. PocketBase renames uploaded files (e.g.
  // tileset.png → tileset_97mrhuar0u.png) and lowercases the original name,
  // so we match each Tiled tileset to its PB file by normalized stem.
  // Tiled may store the image as a relative path (e.g. "dir/tileset.png"),
  // so we match on the basename only.
  const pbTilesets: string[] = record.tilesets || [];
  const tilesets: TilesetAsset[] = (tiled.tilesets || []).map(
    (ts: { name: string; image: string }) => {
      const basename = ts.image.split("/").pop() ?? ts.image;
      const stem = basename.replace(/\.[^.]+$/, "").toLowerCase().replace(/[^a-z0-9]/g, "");
      const pbFile =
        pbTilesets.find((f) => f.toLowerCase().replace(/[^a-z0-9]/g, "").startsWith(stem)) ??
        ts.image;
      return { name: ts.name, url: `${fileBase}/${pbFile}` };
    },
  );

  return { name: record.name, tiledJson, tilesets };
}

// fetchDefaultRecord returns the map marked is_default=true, falling back to
// the map named "main" if none is marked default (backwards compatibility).
async function fetchDefaultRecord(): Promise<MapRecord> {
  const resp = await fetch(
    `${PB_URL}/api/collections/maps/records?filter=(is_default=true)&perPage=1`,
  );
  if (resp.ok) {
    const data = await resp.json();
    if (data.items && data.items.length > 0) {
      return data.items[0] as MapRecord;
    }
  }
  // No is_default map (or PB filter error) — fall back to "main".
  return fetchRecordByName("main");
}

// fetchRecordByName returns the map record with the given name.
async function fetchRecordByName(name: string): Promise<MapRecord> {
  const resp = await fetch(
    `${PB_URL}/api/collections/maps/records?filter=(name="${name}")&perPage=1`,
  );
  if (!resp.ok) throw new Error(`PocketBase responded ${resp.status}`);

  const data = await resp.json();
  if (!data.items || data.items.length === 0) {
    throw new Error(`No map named "${name}" in PocketBase`);
  }
  return data.items[0] as MapRecord;
}
