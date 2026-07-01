# Avatars

> **Status:** skeleton. Records the avatar model and the wire-format gap;
> detailed sprite/composition spec to follow.

This document will specify how avatars are composed, customized, animated, and
described over the wire.

## Model (from `02-functional-requirements.md` § 4)

- Each user and each NPC is represented by an avatar.
- Avatars are **composable**: body shape, skin tone, hair (style + colour),
  outfit, accessory. Persisted in PocketBase `avatar_appearance` (see
  `06-data-model-and-persistence.md`).
- Avatars carry **bubbles** for messages and status (visual speech / status
  indicators above the avatar).
- Idle sprites have a respiration animation; walking speed is mutable via
  characteristics; an avatar can be **attached to another entity** (car, bike,
  vehicle) — see the `Attachment` component in `13-ecs-design.md`.

## Components involved (see `13-ecs-design.md`)

- `AvatarAppearance` — the composable visual definition (replicated).
- `Bubble` — current speech/status bubble (replicated; **core component**, not
  an extension feature).
- `Attachment` — links an avatar to a parent entity (vehicle).
- `Position`, `Velocity` — movement.

## To be specified

- **Wire format** for sending appearance to the World Simulator at login
  (referenced as a gap by `06-data-model-and-persistence.md` § 1).
- Sprite-sheet layout and layered composition (body + hair + outfit +
  accessory) and how Phaser composites them.
- Animation set naming (idle, walk, sit, sleep, emote) and how `PlayAnimation`
  (see `11-replication.md` § 2.4) references them.
- Bubble types (speech, emoji, status) and their lifecycle/duration.

## Open questions

- **[OPEN] Appearance wire format** — protobuf schema for `AvatarAppearance`.
- **[OPEN] Sprite compositing** — pre-baked vs. layered at runtime in Phaser.
- **[OPEN] Bubble rendering** — does chat (`17-chat.md`) drive speech bubbles?
