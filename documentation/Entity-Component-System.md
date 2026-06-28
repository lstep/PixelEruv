---
creation date: 2026-06-26 09:35
modification date: 2026-06-26 09:38
---
Ark ([GitHub - mlange-42/ark: Ark -- Archetype-based Entity Component System (ECS) for Go. · GitHub](https://github.com/mlange-42/ark)) may be used if not developed from scratch.


In a classic object-oriented architecture, a player inherits from a `Character` class, just like an NPC, while an interactive piece of furniture inherits from a `StaticObject` class. This model creates rigid and complex hierarchies as soon as we want to add dynamic interactivity (for example, transforming an office chair into an "NPC" possessed by an AI).

The **ECS** solves this problem by strictly separating data from logic:

- **Entity:** A simple unique identifier (a `uint64` integer). It's just an empty container. Players, bots, meeting tables, and doors are all simple entities.
    
- **Component:** Pure data structures, without algorithmic logic.
    - `Position`: x,y coordinates.
    - `Interactable`: Interaction type (Iframe, Meeting, External link) and trigger radius.
    - `AIBehavior`: AI decision states (Idle, Patrol, Discussion).
    - `NetworkSession`: Link to the user's WebSocket connection (absent for NPCs).
        
- **System:** Algorithms executed at each game loop cycle (_game tick_). They query entities possessing a specific set of components.
    
    - The `MovementSystem` updates all entities that have `Position` + `Velocity`.
        
    - The `TriggerSystem` calculates the Euclidean distance d=sqrt((x2−x1)²+(y2−y1)²)between entities with the `Player` component and those with the `Interactable` component to raise proximity events.
        
    - The `BehaviorTreeSystem` processes decisions of NPCs having the `AIBehavior` component.

