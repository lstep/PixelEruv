# Block-wins resolution for overlapping gate zones

When a player attempts to move into a tile covered by multiple gate-mode zones, the kernel checks all of them: if any returns `block` (cached or via `ask` reply), the movement is refused. Only if all overlapping gate zones permit (or no gate zone covers the tile) is movement allowed.

We chose block-wins over priority layers because it is the simplest rule that is still composable — it matches physical intuition (two overlapping walls are still a wall) and requires no zone ordering. We rejected disallowing overlaps outright because the system's extensibility pitch depends on independent extensions registering adjacent or nested gate zones.

## Consequences

- The kernel caches gate decisions per-zone (not per-tile) and intersects them at evaluation time. For static zones this is O(k) where k is the number of zones containing the tile (typically 1–2).
- For overlapping `ask` zones owned by different extensions, the kernel queries all of them **in parallel** within a per-tick timeout and blocks if any replies `block`. Parallel (not sequential short-circuit) because: (1) sequential requires an ordering decision we have deferred to future priority layers, (2) an audit/observer extension should see all access attempts including those another extension blocks, (3) the wasted-compute cost on a second extension after a first-block is small since `ask` is the minority case. Latency is max(ask₁, ask₂) rather than sum. Slow extensions fail closed to `block` per existing `default_on_timeout` semantics.

## Future: priority layers

Block-wins is the starting point, not necessarily the endpoint. If real-world authoring produces cases where a higher-priority zone should override a lower-priority one (e.g. a "VIP area" inside a "lounge" where the VIP gate should win even if the lounge gate blocks), we will introduce a `priority` field on zones and change the resolution from "intersect all" to "max-priority wins." This is a backward-compatible extension — adding a priority field and a new resolution rule — not a rewrite. The current decision should be revisited once we have authoring experience with overlapping gate zones.
