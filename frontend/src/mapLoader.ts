// Fetches map assets (Tiled JSON + tileset images) from PocketBase.
//
// The `maps` collection (see pb_migrations/) stores the Tiled JSON export and
// tileset images as file fields. This module fetches the record by name,
// retrieves the JSON to extract tileset names, and returns everything GameScene
// needs to load the map via Phaser's loader.
//
// Env vars:
//   VITE_POCKETBASE_URL — PocketBase base URL (default http://localhost:8090)
//   VITE_MAP_NAME       — map record name to load (default "test-map")

const PB_URL = import.meta.env.VITE_POCKETBASE_URL || "http://localhost:8090";
const MAP_NAME = import.meta.env.VITE_MAP_NAME || "test-map";

export interface TilesetAsset {
  // The tileset name as defined in the Tiled JSON (used by addTilesetImage).
  name: string;
  // PocketBase file URL for the tileset image.
  url: string;
}

export interface MapAssets {
  // Parsed Tiled JSON object — passed directly to load.tilemapTiledJSON.
  tiledJson: object;
  // One entry per tileset in the map.
  tilesets: TilesetAsset[];
}

// Minimal shape of a Tiled map JSON — only the fields we read.
interface TiledMap {
  tilesets: { name: string; image: string }[];
}

// Fetch the configured map from PocketBase. Throws if PB is unreachable or the
// map record doesn't exist — the caller should fall back to static files.
export async function loadMapAssets(): Promise<MapAssets> {
  const resp = await fetch(
    `${PB_URL}/api/collections/maps/records?filter=(name="${MAP_NAME}")&perPage=1`,
  );
  if (!resp.ok) throw new Error(`PocketBase responded ${resp.status}`);

  const data = await resp.json();
  if (!data.items || data.items.length === 0) {
    throw new Error(`No map named "${MAP_NAME}" in PocketBase`);
  }

  const record = data.items[0];
  const fileBase = `${PB_URL}/api/files/${record.collectionId}/${record.id}`;

  // Fetch the Tiled JSON so we can extract tileset names and pass the parsed
  // object to Phaser (avoids a double fetch by the loader).
  const tiledJsonUrl = `${fileBase}/${record.tiled_json}`;
  const jsonResp = await fetch(tiledJsonUrl);
  if (!jsonResp.ok) throw new Error(`Failed to fetch Tiled JSON: ${jsonResp.status}`);
  const tiledJson: object = await jsonResp.json();
  const tiled = tiledJson as TiledMap;

  // Build tileset URLs. PocketBase renames uploaded files (e.g.
  // tileset.png → tileset_97mrhuar0u.png), so we match each Tiled tileset to
  // its PB file by filename stem, not exact name.
  const pbTilesets: string[] = record.tilesets || [];
  const tilesets: TilesetAsset[] = (tiled.tilesets || []).map(
    (ts: { name: string; image: string }) => {
      const stem = ts.image.replace(/\.[^.]+$/, "");
      const pbFile = pbTilesets.find((f) => f.startsWith(stem)) ?? ts.image;
      return { name: ts.name, url: `${fileBase}/${pbFile}` };
    },
  );

  return { tiledJson, tilesets };
}
