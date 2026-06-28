---
creation date: 2026-06-26 08:55
modification date: 2026-06-27 18:00
---

### Why Phaser 4 is a "Game-Changer" for your Virtual Office

Virtual offices (like Gather) are characterised by very large office maps,
many static decoration elements, and dozens of collaborators connected
simultaneously. Phaser 4 introduces native GPU features that eliminate the
bottlenecks of Phaser 3:

1. **`TilemapGPULayer` (the end of zoom slowdowns)**: In Phaser 3, zooming out
   to display an entire office floor with thousands of tiles (walls, floors,
   desks) caused a drastic drop in the frame rate. Phaser 4 introduces the
   `TilemapGPULayer`, which renders an entire map layer via a single GPU shader
   as a quad. The render cost becomes **fixed per pixel on screen**,
   independent of the number of visible tiles. You can zoom out to display a
   giant office of 16 million tiles at 60 frames per second with no CPU impact.

2. **`SpriteGPULayer` (crowd handling for avatars)**: This new component pushes
   all sprite render data (avatars, character animations, interactive objects)
   directly into a GPU buffer. Phaser 4 can thus animate and display more than
   one million sprites in a single draw call. This avoids CPU saturation when
   many employees move at the same time.

3. **The Unified Filter system**: To indicate that an employee is in a meeting,
   "busy" (luminous halo), or to darken areas outside the reach of spatial
   audio, Phaser 4 unifies the old complex mask and PostFX systems into a
   single pipeline of stackable, compatible filters. Dynamic shading, focus
   blur, and global lighting now activate in a single line of code without
   impacting the frame budget.

4. **PCT (Phaser Compact Texture) atlases**: This new atlas format replaces
   Phaser 3's heavy JSON files with lightweight textual descriptors, reducing
   their size by 90% to 95%. For an enterprise web application, this means
   near-instant initial load time in employees' browsers.

**Verdict on the engine:** You must go with Phaser 4. The framework keeps a
public API almost identical to Phaser 3 (which avoids losing the community and
compatibility with code-generation AIs like Claude or Cursor), but completely
rewrites the rendering engine for modern WebGL and WebGPU.
