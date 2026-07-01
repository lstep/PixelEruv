# Netcode (latency compensation)

> **Status:** skeleton. The detailed netcode spec is not yet written. This
> file exists so that the many cross-references to it resolve, and it records
> the requirements and the decisions taken so far.

This document will specify the latency-compensation protocol that makes
movement feel instant despite a server-authoritative, ~150 ms round-trip
(see `03-non-functional-requirements.md` § 2).

## Requirements (from `02-functional-requirements.md` § 6)

- **Client-side prediction + server reconciliation** for the local avatar.
- **Snapshot interpolation (LERP)** for remote avatars.
- **Dead reckoning / extrapolation** for disconnection or prolonged loss.

## Decisions taken so far

- **Server tick rate: 20 Hz** (see `03-non-functional-requirements.md` § 3).
- **Input carries a sequence number** (`InputFrame.seq`, see
  `07-network-protocol.md` § 1.3) so the server can echo the last processed
  `seq` for reconciliation.
- **Replication sends full component payloads** (not deltas), which means a
  dropped tick is self-correcting (see `11-replication.md` § 3.4).
- **Local avatar is predicted, not driven by `UpdateComponent`** for its own
  `Position` (see `11-replication.md` § 6).

## To be specified

- Reconciliation algorithm (replay un-acked inputs against the authoritative
  state).
- Snapshot interpolation buffer depth (typically 2–3 ticks).
- Extrapolation / dead-reckoning timeout thresholds.
- How `snapshot_seq` and input `seq` are correlated for reconciliation.
- Clock synchronization between client and server tick estimates.

## Open questions

- **[OPEN] Input cadence** — send on change only, or every tick while held?
- **[OPEN] Interpolation buffer depth** — 2 vs 3 ticks (latency vs smoothness).
- **[OPEN] Extrapolation cap** — how long to dead-reckon a remote avatar before
  freezing it.
