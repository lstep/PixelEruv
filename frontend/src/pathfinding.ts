// findPath — 8-neighbor A* over a tile grid.
//
// Pure (no Phaser deps) so it can be reasoned about and tested standalone.
// The caller supplies a `passable` predicate that already encodes all
// collision sources (Walls tile layer + wall zones expanded by the player
// collision radius). The server remains the movement authority, so a wrong
// passability here yields a suboptimal or stuck path — never a wall-hack.
//
// Diagonal steps are allowed only when both orthogonal neighbors are
// passable (the no-corner-cutting rule). This prevents squeezing through a
// 1-tile diagonal gap between two walls that the server's swept collision
// would reject, keeping the rasterized path conservative relative to the
// continuous-space server check.

export type PassFn = (tx: number, ty: number) => boolean;

export interface Tile { x: number; y: number; }

const SQRT2 = Math.SQRT2;
const ORTHO = 1;
const DIAG = SQRT2;

// 8 neighbors: dx, dy, cost. Orthogonal first, then diagonal.
const NEIGHBORS: ReadonlyArray<readonly [number, number, number]> = [
  [ 1,  0, ORTHO], [-1,  0, ORTHO], [ 0,  1, ORTHO], [ 0, -1, ORTHO],
  [ 1,  1, DIAG ], [ 1, -1, DIAG ], [-1,  1, DIAG ], [-1, -1, DIAG ],
];

// Octile distance heuristic (admissible for an 8-connected grid with
// orthogonal cost 1 and diagonal cost SQRT2).
function heuristic(ax: number, ay: number, bx: number, by: number): number {
  const dx = Math.abs(ax - bx);
  const dy = Math.abs(ay - by);
  return (dx + dy) + (SQRT2 - 2) * Math.min(dx, dy);
}

interface Node {
  x: number;
  y: number;
  g: number; // cost from start
  f: number; // g + heuristic
  parent: number; // index into the node array, -1 for start
}

// Min-binary-heap keyed on node.f. Indices reference the nodes array.
class MinHeap {
  private items: number[] = []; // node indices

  get size(): number { return this.items.length; }

  push(idx: number, f: number[]): void {
    this.items.push(idx);
    this.bubbleUp(this.items.length - 1, f);
  }

  pop(f: number[]): number {
    const n = this.items.length;
    const top = this.items[0];
    const last = this.items.pop()!;
    if (n > 1) {
      this.items[0] = last;
      this.sinkDown(0, f);
    }
    return top;
  }

  private bubbleUp(i: number, f: number[]): void {
    const items = this.items;
    const idx = items[i];
    const key = f[idx];
    while (i > 0) {
      const parent = (i - 1) >> 1;
      if (key >= f[items[parent]]) break;
      items[i] = items[parent];
      i = parent;
    }
    items[i] = idx;
  }

  private sinkDown(i: number, f: number[]): void {
    const items = this.items;
    const n = items.length;
    const idx = items[i];
    const key = f[idx];
    for (;;) {
      const l = 2 * i + 1;
      const r = l + 1;
      let smallest = i;
      let smallestKey = key;
      if (l < n && f[items[l]] < smallestKey) { smallest = l; smallestKey = f[items[l]]; }
      if (r < n && f[items[r]] < smallestKey) { smallest = r; }
      if (smallest === i) break;
      items[i] = items[smallest];
      i = smallest;
    }
    items[i] = idx;
  }
}

// findPath returns the list of tiles from start (exclusive) to goal
// (inclusive), or null if no path exists. `w`/`h` are the grid bounds;
// tiles outside [0,w) x [0,h) are treated as impassable by the caller's
// `passable` predicate (which should also bounds-check).
export function findPath(
  startX: number, startY: number,
  goalX: number, goalY: number,
  passable: PassFn,
): Tile[] | null {
  if (!passable(goalX, goalY)) return null;
  if (startX === goalX && startY === goalY) return [{ x: goalX, y: goalY }];

  const nodes: Node[] = [];
  const f: number[] = []; // f-score per node, indexed in parallel with nodes
  const indexByKey = new Map<number, number>(); // key = y*100000+x → node index
  const closed = new Set<number>();

  const key = (x: number, y: number) => y * 100000 + x;

  const startIdx = nodes.length;
  nodes.push({ x: startX, y: startY, g: 0, f: heuristic(startX, startY, goalX, goalY), parent: -1 });
  f.push(nodes[0].f);
  indexByKey.set(key(startX, startY), startIdx);

  const open = new MinHeap();
  open.push(startIdx, f);

  while (open.size > 0) {
    const curIdx = open.pop(f);
    const cur = nodes[curIdx];
    const curKey = key(cur.x, cur.y);
    if (closed.has(curKey)) continue;
    closed.add(curKey);

    if (cur.x === goalX && cur.y === goalY) {
      // Reconstruct path (goal inclusive, start exclusive).
      const path: Tile[] = [];
      let i: number = curIdx;
      while (i !== startIdx) {
        const n = nodes[i];
        path.push({ x: n.x, y: n.y });
        i = n.parent;
      }
      path.reverse();
      return path;
    }

    for (const [dx, dy, cost] of NEIGHBORS) {
      const nx = cur.x + dx;
      const ny = cur.y + dy;
      const nKey = key(nx, ny);
      if (closed.has(nKey)) continue;
      if (!passable(nx, ny)) continue;

      // No corner cutting: a diagonal step requires both orthogonal
      // neighbors to be passable, so the path can't squeeze through a
      // 1-tile diagonal gap.
      if (dx !== 0 && dy !== 0) {
        if (!passable(cur.x + dx, cur.y) || !passable(cur.x, cur.y + dy)) continue;
      }

      const g = cur.g + cost;
      const existing = indexByKey.get(nKey);
      if (existing === undefined) {
        const idx = nodes.length;
        nodes.push({ x: nx, y: ny, g, f: g + heuristic(nx, ny, goalX, goalY), parent: curIdx });
        f.push(nodes[idx].f);
        indexByKey.set(nKey, idx);
        open.push(idx, f);
      } else if (g < nodes[existing].g) {
        // Found a cheaper route to an already-seen node. Update in place
        // and re-push (lazy deletion handles the stale entry).
        nodes[existing].g = g;
        nodes[existing].f = g + heuristic(nx, ny, goalX, goalY);
        nodes[existing].parent = curIdx;
        f[existing] = nodes[existing].f;
        open.push(existing, f);
      }
    }
  }

  return null;
}
