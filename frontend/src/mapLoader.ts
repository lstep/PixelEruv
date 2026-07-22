// Fetches map assets (Tiled JSON + tileset images) from worldsim's asset API.
//
// worldsim serves map metadata, the parsed Tiled JSON, and tileset file URLs
// via GET /api/assets/maps/{name} (or /api/assets/maps/default). The maps PB
// collection is fully locked (nil API rules), so the frontend cannot hit
// /api/collections/maps/ directly. worldsim reads the data via the in-process
// Go SDK (bypassing rules) and serves it on its embedded PB router.
//
// When no map name is given, the map marked is_default=true is loaded (this is
// where new players spawn). Falls back to name "main" for backwards
// compatibility.
//
// Env vars:
//   VITE_POCKETBASE_URL — worldsim/PocketBase base URL (dev only; default http://localhost:8090)
//
// In production (served by nginx), PB_URL derives from window.location.origin so
// the browser reaches worldsim via the nginx /api/ proxy (same-origin), which
// works for both localhost and remote access without hardcoded addresses.

const PB_URL = window.location.port === "5173"
  ? (import.meta.env.VITE_POCKETBASE_URL ?? "http://localhost:8090")
  : window.location.origin;

export interface TilesetAsset {
  // The tileset name as defined in the Tiled JSON (used by addTilesetImage).
  name: string;
  // URL to the tileset image (served by worldsim's asset endpoint).
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

// Response shape from GET /api/assets/maps/{name}.
interface MapAssetsResponse {
  name: string;
  tiledJson: object;
  tilesets: { name: string; url: string }[];
}

// Fetch the configured map from worldsim. Throws if the endpoint is
// unreachable or the map record doesn't exist — the caller should fall back to
// static files. If mapName is omitted, loads the default map (is_default=true,
// falling back to "main").
export async function loadMapAssets(mapName?: string): Promise<MapAssets> {
  const endpoint = mapName
    ? `${PB_URL}/api/assets/maps/${encodeURIComponent(mapName)}`
    : `${PB_URL}/api/assets/maps/default`;

  const resp = await fetch(endpoint);
  if (!resp.ok) throw new Error(`Failed to fetch map assets: ${resp.status}`);

  const data: MapAssetsResponse = await resp.json();

  // The server returns relative tileset URLs (e.g. /api/assets/maps/main/tilesets/foo.png).
  // Prepend PB_URL so Phaser's loader can fetch them.
  const tilesets: TilesetAsset[] = (data.tilesets || []).map((ts) => ({
    name: ts.name,
    url: ts.url.startsWith("http") ? ts.url : `${PB_URL}${ts.url}`,
  }));

  return { name: data.name, tiledJson: data.tiledJson, tilesets };
}
