# CLAUDE.md

Behavioral guidelines to reduce common LLM coding mistakes. Merge with project-specific instructions as needed.

**Tradeoff:** These guidelines bias toward caution over speed. For trivial tasks, use judgment.

## Approach
- Read existing files before writing. Don't re-read unless changed.
- Thorough in reasoning, concise in output.
- Skip files over 100KB unless required.
- No sycophantic openers or closing fluff.
- No emojis or em-dashes.
- Do not guess APIs, versions, flags, commit SHAs, or package names. Verify by reading code or docs before asserting.

## 1. Think Before Coding

**Don't assume. Don't hide confusion. Surface tradeoffs.**

Before implementing:
- State your assumptions explicitly. If uncertain, ask.
- If multiple interpretations exist, present them - don't pick silently.
- If a simpler approach exists, say so. Push back when warranted.
- If something is unclear, stop. Name what's confusing. Ask.

## 2. Simplicity First

**Minimum code that solves the problem. Nothing speculative.**

- No features beyond what was asked.
- No abstractions for single-use code.
- No "flexibility" or "configurability" that wasn't requested.
- No error handling for impossible scenarios.
- If you write 200 lines and it could be 50, rewrite it.

Ask yourself: "Would a senior engineer say this is overcomplicated?" If yes, simplify.

## 3. Surgical Changes

**Touch only what you must. Clean up only your own mess.**

When editing existing code:
- Don't "improve" adjacent code, comments, or formatting.
- Don't refactor things that aren't broken.
- Match existing style, even if you'd do it differently.
- If you notice unrelated dead code, mention it - don't delete it.

When your changes create orphans:
- Remove imports/variables/functions that YOUR changes made unused.
- Don't remove pre-existing dead code unless asked.

The test: Every changed line should trace directly to the user's request.

## 4. Goal-Driven Execution

**Define success criteria. Loop until verified.**

Transform tasks into verifiable goals:
- "Add validation" → "Write tests for invalid inputs, then make them pass"
- "Fix the bug" → "Write a test that reproduces it, then make it pass"
- "Refactor X" → "Ensure tests pass before and after"

For multi-step tasks, state a brief plan:
```
1. [Step] → verify: [check]
2. [Step] → verify: [check]
3. [Step] → verify: [check]
```

Strong success criteria let you loop independently. Weak criteria ("make it work") require constant clarification.

## 5. Delegation

- Use read-only subagents for broad repo research.
- Use implementation subagents only for isolated work.
- Use the review skill before opening a PR.

## 6. Memory

- Save durable facts only: setup, recurring commands, architecture decisions,
  test accounts, and user preferences.
- Never store secrets.

**These guidelines are working if:** fewer unnecessary changes in diffs, fewer rewrites due to overcomplication, and clarifying questions come before implementation rather than after mistakes.

## Reporting

When reporting information to me, be extremely concise and sacrifice grammar for the sake of concision.


## Project-Specific Guidelines

### Prerequisites (read before building or testing)

- **Proto codegen is required before anything compiles.** `backend/internal/pb/` is gitignored (`*.pb.go`) and empty by default. Run `make proto` to generate Go + TypeScript from `proto/*.proto`. Without this, `go build` and `go test` fail everywhere.
  ```bash
  make proto   # requires protoc installed; outputs to backend/internal/pb/ and frontend/src/proto/
  ```
- Go 1.26.4 (per go.mod). Protoc and buf must be installed and in PATH — `make build` depends on `make proto` to regenerate the gitignored `.pb.go` files before compiling.

### Architecture overview

```
Browser ──WS──> Nginx ──> Pusher ──NATS──> WorldSim ──> PocketBase
                                ↕               ↕
                             ext-demo        ext-walls
                             ext-props       ext-av ──> LiveKit
```

- **Pusher** (`backend/cmd/pusher`): WebSocket ↔ NATS gateway. Validates PocketBase JWTs via the PB API, forwards frames to/from worldsim. Pure passthrough — knows nothing about replication wire format.
- **WorldSim** (`backend/cmd/worldsim`): Spatial authority + ECS kernel. Owns entities, zones, collision, replication. Only gameplay system is avatar movement. Emits `worldsim.ready` on NATS after subscriptions are live.
- **Extensions** (`ext-demo`, `ext-walls`, `ext-props`, `ext-av`): Peer processes on the NATS bus. Register triggers via `extension.<id>.register`. All gameplay logic lives here, not in the kernel.
- **PocketBase**: Maps, players, positions storage. Worldsim hits it for map data, user lookup, position persistence.
- **MCP** (`backend/cmd/mcp`): Model Context Protocol server exposing worldsim state, audit history, and admin actions (kick/ban/teleport/chat-as/set_*) to LLM clients over HTTP/SSE on `:8085/mcp`. Bearer-token auth (`MCP_AUTH_TOKEN`, required). Separate binary from worldsim to isolate MCP load from the game loop. See `documentation/plans/2026-07-19-mcp-server-design.md`.
- **Frontend** (`frontend/`): Phaser 4 client (TypeScript/Vite). PocketBase auth (email/password + OAuth2), sprite rendering, WebSocket client.

### Build and test commands

```bash
# Generate protobuf (REQUIRED before build/test)
make proto

# Build all Go binaries into dist/bin/
make build

# Run worldsim unit tests (no Docker needed)
cd backend && go test ./internal/worldsim/ -v

# Integration tests (require Docker stack running: nats + pocketbase)
cd backend && go test ./test/integration/ -v

# Start full local dev stack (nats + pocketbase + mailhog + pusher + worldsim)
make up

# Stop everything
make down

# Debug with OpenTelemetry + motel (installs motel if missing)
make debug    # starts motel, NATS container, PocketBase, worldsim + pusher with OTEL_ENABLED
```

### Testing notes

- Tests use Go's standard `testing` package — **not Ginkgo**. Do not try to run `ginkgo`.
- Integration tests start an in-process pusher via `TestMain` (no PocketBase auth configured, `IdToken="dev"` works). They still need NATS at `localhost:4222`.
- Worldsim unit tests run without Docker — they test the ECS, zones, collision, replication, chat, name tags, etc. in isolation.
- Integration tests cover: guest auth, keepalive, lite MVP (auth + input + replication flow).

### Git workflow

- Do not commit in branch `main`. Check you are in a branch and if not, create one.
- Commit locally as you go, but do NOT push automatically.
- Only push and create a PR when explicitly asked. Never push directly to main — always create a PR.

### Other

- Keep a DASHBOARD.md up to date: progress, what remains, decisions made. Update it at the end of each session.
- Before planning, query memory for stable facts about this repo. At the end, save only durable facts that will help future sessions. Do not save secrets, logs, guesses, or one-off errors.
- Worldsim auto-seeds sprite_bases from `SPRITES_DIR` (default `./sprites`) on startup — non-fatal if it fails.
- Heredoc chokes on backticks. Write the body to a file first.

### Frontend camera zoom (Phaser 4)

The GameScene camera zoom is user-adjustable via mouse wheel (ZOOM_MIN=1, ZOOM_MAX=4, default 2). **Any UI element positioned in world space must account for zoom**, or it will end up at the wrong screen position and/or scale with zoom (which also breaks input hit areas).

Established pattern (see `openDropdown`, name tags at `GameScene.ts` ~L1222):
- Place the container at world coordinates (e.g. `cam.worldView.centerX/Y` for screen-center, or `avatar.sprite.x/y` to follow an entity).
- Counter-scale: `container.setScale(1 / cam.zoom)` so it renders at constant screen size.
- If the element should persist while the camera pans/zooms, re-apply position + scale every frame in `update()` (outside any per-avatar loop).
- **Do NOT use `setScrollFactor(0)` to "fix" zoom** — in Phaser 4 it does not bypass camera zoom, and it makes Phaser misinterpret world-coordinate positions as screen coordinates. Only use `setScrollFactor(0)` for elements whose position is already in screen space (e.g. `this.scale.width/2`, `0,0` top-left), like the disconnect overlay.
- Verify with the wheel zoom while the element is visible: it must stay at the same screen position and not resize.

## Browser Automation

Use `agent-browser` for web automation. Run `agent-browser --help` for all commands.

Core workflow:

1. `agent-browser open <url>` - Navigate to page
2. `agent-browser snapshot -i` - Get interactive elements with refs (@e1, @e2)
3. `agent-browser click @e1` / `fill @e2 "text"` - Interact using refs
4. Re-snapshot after page changes
