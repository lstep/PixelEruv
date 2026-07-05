# PixelEruv.o — Dashboard

Dernière mise à jour : 2026-07-05 (session 2)

## Vue d'ensemble

MMO spatial 2D top-down avec authentification OIDC, identité persistante,
système de zones extensible et extensions first-party. Architecture kernel
(worldsim + pusher) + extensions communicantes via NATS.

## Architecture actuelle

```
Browser ──WS──> Nginx ──> Pusher ──NATS──> WorldSim ──> PocketBase
                                ↕               ↕
                             ext-demo        ext-walls
```

| Service      | Rôle                                              | Stack         |
|--------------|---------------------------------------------------|---------------|
| frontend     | Client Phaser 3, auth OIDC, rendu sprites         | TypeScript/Vite |
| pusher       | Passerelle WebSocket ↔ NATS, validation JWT       | Go            |
| worldsim     | Autorité spatiale, ECS, zones, réplication        | Go            |
| pocketbase   | Stockage maps, joueurs, positions                 | PocketBase    |
| dex          | Fournisseur d'identité OIDC (local-password)      | Dex           |
| nats         | Bus de messages pub/sub + JetStream               | NATS          |
| ext-demo     | Extension de démonstration (log zone events)      | Go            |
| ext-walls    | Extension murs (gate triggers block sur zones)    | Go            |

## Fonctionnalités implémentées

### Authentification & Identité
- [x] Dex OIDC avec authorization code flow + PKCE
- [x] Validation JWT côté pusher (JWKS, iss, aud, sub)
- [x] 2 utilisateurs : `admin@pixeleruv.local` / `player@pixeleruv.local` (mdp: `password123`)
- [x] Identité persistante : `oidc_sub` → record PocketBase `players` → `entity_id` + position
- [x] Position sauvegardée sur déconnexion, restaurée à la reconnexion

### Rendu & Mouvement
- [x] Sprites personnages 32x32 (6 personnages, 4 directions, 6 frames walk)
- [x] Animations walk (3fps) + idle (2fps, 4 frames)
- [x] Mapping directions : 0=down, 1=left, 2=right, 3=up
- [x] Mouvement 8-directionnel avec slide le long des murs
- [x] Collision : tile layer Walls (fallback) + gate triggers extension (zones)

### Zones & Extensions
- [x] Parsing Zones object layer depuis Tiled (rect, circle, polygon)
- [x] Pré-rastérisation des zones statiques (lookup O(1) par tile)
- [x] Détection enter/exit → publication NATS `zone.enter` / `zone.exit`
- [x] Protocole extension : `extension.<id>.register`, `.heartbeat`, `.register_triggers`
- [x] Gate triggers : `block` / `allow` (cache local, évalués pendant le mouvement)
- [x] Détection d'extensions stale (3× heartbeat interval)
- [x] ext-walls : lit la map, trouve `zone_type=wall`, enregistre des triggers block
- [x] ext-demo : log les événements zone enter/exit
- [x] Murs migrés vers le système d'extensions (Walls tile layer = fallback uniquement)

### Intégrité & Documentation
- [x] Map integrity checker : validation au démarrage, toutes les 5 min, et à la demande (`admin.map.integrity` via NATS)
- [x] Documentation map design guide (`documentation/21-map-design-guide.md`) : layers, propriétés, shapes, upload
- [x] Diagramme SVG de la structure des layers et du flux de données (`documentation/map-design-guide.html`)

### Infrastructure
- [x] Docker Compose : nats, pocketbase, dex, pusher, worldsim, frontend, ext-demo, ext-walls
- [x] Nginx proxy : `/dex/` → Dex (same-origin pour le browser)
- [x] Makefile pour dev local (pusher + worldsim en binaire natif)
- [x] OpenTelemetry instrumentation (désactivé par défaut)

## Ce qui reste (MVP)

### Priorité haute
- [ ] **Camera follow** : la caméra suit le joueur local au lieu de montrer toute la map
- [ ] **Zones dans Tiled** : ajouter des rectangles sur l'object layer Zones avec `zone_type=wall` pour tester ext-walls
- [ ] **Chat** : UI chat + collection PocketBase, messages broadcast via NATS

### Priorité moyenne
- [ ] **LiveKit A/V** : audio/vidéo positionnel (serveur LiveKit, bridge, token exchange, WebRTC client)
- [ ] **AOI filter** : ne répliquer que les entités dans le rayon du client + même zone
- [ ] **Input triggers** : clics/touches → broadcast aux extensions (interactions NPC, objets)
- [ ] **Exclusive zones** : isolation visuelle + audio pour les membres

### Priorité basse
- [ ] **Knock-to-join** : meeting rooms avec propriétaire et admission
- [ ] **Mobile zones** : zones circulaires qui suivent une entité (vision de PNJ)
- [ ] **Extension pack complet** : walls, doors, base zone behaviors, base triggers
- [ ] **Prédiction côté client + réconciliation** (netcode-lerp-prediction branch existe)

## Décisions architecturales

| Date       | Décision | Rationale |
|------------|----------|-----------|
| 2026-07-05 | Authorization code flow + PKCE (pas implicit) | Dex ne supporte pas `response_type=id_token` |
| 2026-07-05 | Collection `players` (pas `users`) | PocketBase a déjà une collection `users` intégrée |
| 2026-07-05 | Auth superuser pour API PocketBase | Les règles `null` = superuser only pour create/update |
| 2026-07-05 | `DEX_ISSUER` séparé de `DEX_JWKS_URL` | Token `iss` = `localhost:5556`, mais pusher atteint Dex via `dex:5556` en Docker |
| 2026-07-05 | Zones = object layer Tiled (pas tile layer) | Les zones sont des formes vectorielles avec metadata, pas des tiles |
| 2026-07-05 | Gate triggers en cache local (pas round-trip NATS) | `block`/`allow` sont déterministes, pas besoin de requêter l'extension à chaque mouvement |
| 2026-07-05 | Walls tile layer conservé comme fallback | Évite de casser la collision si aucune zone wall n'est définie |
| 2026-07-05 | Re-registration périodique des extensions | NATS Core est fire-and-forget ; le premier publish peut être perdu |
| 2026-07-05 | Murs migrés vers extensions (gate triggers) | Architecture kernel sans gameplay logic ; Walls tile layer conservé comme fallback |
| 2026-07-05 | Integrity checker au démarrage + périodique + à la demande | Détecte corruption/incohérences de map tôt et pendant l'exécution |

## Comptes de test

| Service      | Identifiant                | Mot de passe   |
|--------------|----------------------------|----------------|
| Dex admin    | `admin@pixeleruv.local`    | `password123`  |
| Dex player   | `player@pixeleruv.local`   | `password123`  |
| PB superuser | `admin@pixeleruv.local`    | `password123`  |

## Commandes utiles

```bash
# Dev local
make up                    # démarre nats + pocketbase + dex + pusher + worldsim
make down                  # arrête tout

# Docker (full stack)
docker compose -f docker/docker-compose.yml up --build -d
docker compose -f docker/docker-compose.yml logs -f worldsim
docker compose -f docker/docker-compose.yml restart ext-walls

# PocketBase admin
http://localhost:8090/_/

# Vérifier les joueurs enregistrés
curl -s http://localhost:8090/api/collections/players/records | jq

# Logs zone events
docker logs pixeleruv-ext-demo-1 -f

# Map integrity check à la demande
nats -s nats://localhost:4222 pub admin.map.integrity ""
docker logs pixeleruv-worldsim-1 2>&1 | grep "integrity"
```

## Branches notables

| Branche                  | Description |
|--------------------------|-------------|
| main                     | Branche principale |
| zones                    | Zones + extension protocol (merged into main) |
| netcode-lerp-prediction  | Prédiction client + interpolation (non mergée) |
