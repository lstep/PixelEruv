# Roadmap: Periodic World Snapshot on the Welcome Page

## Goal

Show a periodically refreshed (60s) full-map screenshot of the current world
(map tiles + props in their current state + live player positions) below the
buttons on the public Welcome page (`/welcome/`), so visitors get a live
glimpse of the world before entering.

## Context

- The Welcome page is a static HTML file at `docker/welcome/index.html`, baked
  into the `frontend` nginx container and served from `/var/www/welcome/`
  (see `docker/nginx.conf` `location /welcome`). It currently shows a cover
  image, an `<h1>`, a paragraph, a `.links` row (Enter the world / Register /
  Admin login), and a community footer.
- The world is rendered **only client-side** by Phaser 4
  (`frontend/src/scenes/GameScene.ts`). There is no server-side renderer.
  Worldsim owns the spatial authority + ECS (entities, positions, prop
  state) and the map data (Tiled JSON + tileset PNGs in PocketBase), but it
  does not produce images.
- The audit service has a `/world` endpoint, but it renders textual stats
  via Go templates + HTMX polling, not an image.

## Approaches Considered (and Deferred)

### A. Headless browser snapshot container

A new Docker service running headless Chrome + Puppeteer. The frontend gains
a `?snapshot=1` mode: auto-connect as a guest, hide all DOM UI (TopMenu,
ChatPanel, welcome icon), wait for map load + replication, set the camera to
fit the full map (`setBounds` + `setZoom` to fit), hide name tags, then
expose a readiness sentinel. The Puppeteer script loads the URL, waits for
the canvas, screenshots the canvas element, writes the PNG to a shared
volume, sleeps 60s, loops. nginx serves the PNG from the volume; the welcome
HTML gets an `<img>` with a cache-busting query param.

- **Pros:** faithful — shows map + props + players + lighting exactly as a
  player sees it; reuses all existing Phaser rendering; no second rendering
  implementation to maintain.
- **Cons:** adds a ~300MB Chrome image to the stack; one extra guest session
  always connected (minor load on worldsim/pusher); snapshot-mode code path
  lives in the frontend.

### B. Go server-side render in worldsim

A periodic goroutine in worldsim (or a new `ext-snapshot` extension) reads
the Tiled JSON + tileset PNGs from PocketBase (already cached) and renders
tile layers + base entities (props) to a PNG using `image/draw`. Player
positions are overlaid as colored dots (full sprite-sheet rendering with
correct walk frames is substantially more code and diverges from the
client's animation state). Writes the PNG to a shared volume; same nginx +
welcome HTML integration as A.

- **Pros:** lightweight — no Chrome, no extra client session; self-contained
  in Go; trivial to schedule (ticker).
- **Cons:** re-implements tile rendering (layers, gid → tileset lookup,
  blit); players as dots, not sprites, unless we also render sprite sheets;
  no animations, no lighting glow, no name tags; the image diverges from
  what a player actually sees.

## Decision

**Defer both A and B. Find another way.** Both approaches are technically
sound but either heavyweight (A) or a divergent re-implementation (B). We
want a path that is lightweight *and* faithful, or that sidesteps the
rendering question entirely.

## Alternatives to Explore

1. **SVG/vector minimap driven by the audit stats channel.** Worldsim
   already exposes `worldsim.stats.get` over NATS (request-reply) with
   entity positions and counts. Render a lightweight SVG minimap (map
   outline from collision grid + player dots + prop on/off markers) and
   serve it as an `<img>` or inline SVG. No tile blitting, no Chrome, no
   sprite sheets. Less "screenshot", more "live minimap" — but cheap and
   honest about what it is.

2. **Real-client canvas upload via NATS.** Have an opted-in client (an
   admin's own browser, or a dedicated "camera" player) periodically call
   `canvas.toDataURL()` and upload the PNG to a new PocketBase file
   collection or a NATS subject that an extension writes to disk. Reuses
   the real Phaser render with no Chrome container; the cost is one real
   browser tab staying open. Could be a userscript/browser extension so it
   needs no first-class frontend mode.

3. **Tiled "export as image" + live player overlay.** Cache a static
   full-map render of the tile layers (produced once at build time or on
   map upload) and overlay live player dots + prop state markers fetched
   from `worldsim.stats.get`. Cheap, mostly static, but the map portion is
   always current-on-reupload and the dots are live. Similar in spirit to
   #1 but with a real tile-image background instead of vector outlines.

4. **Headless browser, but shared with another host.** If a Chrome
   container is unavoidable for faithfulness, investigate running it on a
   separate host (or serverless/ephemeral) triggered by a NATS request
   rather than as a always-on stack member, so the main stack stays light.

## Open Questions

- Which alternative is acceptable to the project? (1 is cheapest, 2 is most
  faithful, 3 is a middle ground, 4 keeps faithfulness off the main host.)
- Must players be rendered as real sprites, or are colored dots/markers OK?
- Where does the image live: a shared Docker volume mounted into the
  frontend container, or a PocketBase file collection served via `/api/`?
  A volume is simpler; PB integrates with the existing admin UI and auth.
- Cache-busting on the welcome page: a JS `Date.now()` query param on the
  `<img src>` refreshed every 60s, or an HTTP `Cache-Control: no-cache` on
  the snapshot URL? The welcome page is static, so JS-side refresh is the
  natural fit.
- Snapshot dimensions: full map at 1:1 (could be large), or a fixed
  max-width (e.g. 600px, matching `.cover`) with the map scaled to fit?

## Welcome Page Integration (Approach-Independent)

Regardless of which alternative is chosen, the Welcome page change is the
same: add an `<img>` below the `.links` div, above the `.community` footer,
pointing at the snapshot URL with a cache-busting query param updated by a
small inline `<script>` every 60s. Example shape:

```html
<div class="snapshot">
  <img id="world-snapshot" src="/welcome/snapshots/world.png" alt="Live world snapshot">
</div>
<script>
  (function () {
    var img = document.getElementById('world-snapshot');
    function refresh() { img.src = '/welcome/snapshots/world.png?t=' + Date.now(); }
    setInterval(refresh, 60000);
  })();
</script>
```

nginx serves `/welcome/snapshots/` from a shared volume (or proxies to a PB
file URL). A `.snapshot` CSS rule (max-width 600px, border-radius 12px,
margin-top 1.5rem) matches the existing `.cover` styling.
