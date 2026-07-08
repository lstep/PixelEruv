# CLAUDE.md

Behavioral guidelines to reduce common LLM coding mistakes. Merge with project-specific instructions as needed.

**Tradeoff:** These guidelines bias toward caution over speed. For trivial tasks, use judgment.

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



## Project-Specific Guidelines

### Prerequisites (read before building or testing)

- **Proto codegen is required before anything compiles.** `backend/internal/pb/` is gitignored (`*.pb.go`) and empty by default. Run `make proto` to generate Go + TypeScript from `proto/*.proto`. Without this, `go build` and `go test` fail everywhere.
  ```bash
  make proto   # requires protoc installed; outputs to backend/internal/pb/ and frontend/src/proto/
  ```
- Go 1.26.4 (per go.mod). Protoc and buf are not in PATH — install them if `make proto` fails.

### Architecture overview

```
Browser ──WS──> Nginx ──> Pusher ──NATS──> WorldSim ──> PocketBase
                                ↕               ↕
                             ext-demo        ext-walls
                             ext-props       ext-av ──> LiveKit
```

- **Pusher** (`backend/cmd/pusher`): WebSocket ↔ NATS gateway. Validates JWTs from Dex OIDC, forwards frames to/from worldsim. Pure passthrough — knows nothing about replication wire format.
- **WorldSim** (`backend/cmd/worldsim`): Spatial authority + ECS kernel. Owns entities, zones, collision, replication. Only gameplay system is avatar movement. Emits `worldsim.ready` on NATS after subscriptions are live.
- **Extensions** (`ext-demo`, `ext-walls`, `ext-props`, `ext-av`): Peer processes on the NATS bus. Register triggers via `extension.<id>.register`. All gameplay logic lives here, not in the kernel.
- **PocketBase**: Maps, players, positions storage. Worldsim hits it for map data, user lookup, position persistence.
- **Frontend** (`frontend/`): Phaser 4 client (TypeScript/Vite). OIDC auth, sprite rendering, WebSocket client.

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

# Start full local dev stack (nats + pocketbase + dex + pusher + worldsim)
make up

# Stop everything
make down

# Debug with OpenTelemetry + motel (installs motel if missing)
make debug    # starts motel, NATS container, PocketBase, worldsim + pusher with OTEL_ENABLED
```

### Testing notes

- Tests use Go's standard `testing` package — **not Ginkgo**. Do not try to run `ginkgo`.
- Integration tests start an in-process pusher via `TestMain` (no Dex configured, `IdToken="dev"` works). They still need NATS at `localhost:4222`.
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
