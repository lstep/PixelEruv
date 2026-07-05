# limezu Character Sprites — Layout & Pitfalls

> **Read this before touching character sprite loading in
> `frontend/src/scenes/GameScene.ts`.** It documents a non-obvious quirk of
> the limezu "Modern Interiors" character sheets that has already caused one
> bug (heads rendered cropped).

## Source

Characters come from the limezu **Modern Interiors** pack:
`assets/moderninteriors-win/2_Characters/`. We use the 32px legacy single
character sheets (`Old/Single_Characters_Legacy/32x32/`) as the art source.

The in-game sheets are `frontend/public/sprites/char_0.png` … `char_5.png`,
each **768×192** px.

## The key quirk: characters are ~48px tall, not 32px

A limezu character is **taller than one 32px tile** (~48px). In the sheet the
character is split across **two** 32px rows:

- the **head/hair top** sits in the bottom ~16px of one 32px row (the "even"
  row), and
- the **face + body + legs** fill the entire next 32px row (the "odd" row).

Verified vertical content extent (non-transparent bbox) per 32px cell:

```
row 0 (even): y[16-32]   <- head-top only (idle, few cols)
row 1 (odd) : y[0-32]    <- body (idle)
row 2 (even): y[16-32]   <- head-top only (walk)
row 3 (odd) : y[0-32]    <- body (walk)
row 4 (even): y[14-32]   <- head-top only
row 5 (odd) : y[0-32]    <- body
```

So a **complete** character = even-row (head) **+** odd-row (body) stacked.

### The bug this caused

Slicing the sheet into **32×32** frames captures only the odd (body) row, so
the top of the head is cut off flat. This looked like a rendering/depth/origin
problem and several fixes were attempted in the wrong place (setDepth,
setOrigin, camera) before the real cause — the frame height — was found.

## The correct way to slice

Load the sheet as **32×64** frames (24 cols × 3 rows). Each 64px frame spans
two physical 32px rows and therefore contains a full head + body.

```ts
const FRAME_W = 32;
const FRAME_H = 64;          // NOT 32 — captures the tall character
const COLS_PER_ROW = 24;

this.load.spritesheet(key, `/sprites/${key}.png`, {
  frameWidth: FRAME_W,
  frameHeight: FRAME_H,
});
```

Frame index = `frameRow * COLS_PER_ROW + col`. With 64px frames:

| frame-row | physical rows | animation           |
|-----------|---------------|---------------------|
| 0         | 0–1           | idle (cols 0–3 only)|
| 1         | 2–3           | **walk** (all cols) |
| 2         | 4–5           | **run** (all cols)  |

The frontend currently uses frame-row 2 (run) as the default movement
animation, controlled by `WALK_ROW` in `GameScene.ts`.

Walk-cycle column layout (per direction, 6 frames each):

| direction | cols  | start index (frame-row 1) |
|-----------|-------|---------------------------|
| right     | 0–5   | 24                        |
| up        | 6–11  | 30                        |
| left      | 12–17 | 36                        |
| down      | 18–23 | 42                        |

(Dir field from the server: `0=down, 1=left, 2=right, 3=up`.)

## Positioning / origin

Because the character is 48px tall inside a 64px frame (content occupies frame
y16–64, i.e. sitting at the bottom), use:

```ts
sprite.setOrigin(0.5, 0.75);
```

With the sprite placed at the tile **center** (`x*32+16, y*32+16`), origin
`0.75` puts the **feet at the tile bottom** and lets the head extend ~16px up
into the tile above — the standard "tall character" top-down look. This keeps
the existing tile-center position formulas unchanged.

## Malformed sheet: `char_5`

`char_5.png` is **broken** — its walk-cycle rows only contain the right/up
directions; the down and left frames are empty. It renders as an invisible/
empty sprite and is therefore **excluded** from `CHAR_SPRITES`. If you want it
back, regenerate it from the source with the correct direction layout.

## Checklist before changing sprite code

1. Frames must be **32×64**, not 32×32.
2. Walk cycles are in **frame-row 1** (indices 24–47), not physical row 3.
3. Origin `(0.5, 0.75)` with tile-center positioning.
4. Verify new/regenerated sheets have all four directions in the walk rows
   (check with a quick PIL bbox dump per cell before wiring them in).
