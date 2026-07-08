# Plan: Update AGENTS.md

## Goal
Rewrite `/home/code2/projects/PixelEruv/AGENTS.md` to be a compact, high-signal instruction file for future OpenCode sessions.

## What changes

### Keep (unchanged)
- All 6 behavioral guidelines sections (1-6) — these are useful and repo-appropriate.

### Fix
- **"Use ginkgo for testing"** → Tests use Go's standard `testing` package (verified in `backend/test/integration/main_test.go`, `worldsim_*_test.go`). Ginkgo is not used.

### Add (new sections)
1. **Prerequisites** — Proto codegen is required before anything compiles. `backend/internal/pb/` is gitignored and empty by default. Must run `make proto`.
2. **Architecture overview** — ASCII diagram + one-liner per service (pusher, worldsim, extensions, pocketbase, frontend).
3. **Build and test commands** — Exact commands for proto, build, unit tests, integration tests, dev stack, debug with OTEL.
4. **Testing notes** — Standard `testing` (not Ginkgo), integration test prerequisites (NATS required), unit tests don't need Docker.
5. **Tooling prerequisites** — Go 1.26.4, protoc not in PATH.

### Remove
- "Use ginkgo for testing" (wrong)
- Vague "Before planning, query memory..." line is fine but move to "Other" section

### Preserve
- Git workflow (don't commit on main, PR only) — accurate and important
- DASHBOARD.md reference — useful
- Memory guideline — useful
