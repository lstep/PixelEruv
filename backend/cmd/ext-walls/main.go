// ext-walls is a first-party extension that registers block gate triggers
// on wall zones. It reads the Tiled map from PocketBase, finds zones with
// zone_type "wall", and tells the worldsim to block movement into them.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"
)

type tiledMapJSON struct {
	Layers []struct {
		Name    string `json:"name"`
		Type    string `json:"type"`
		Objects []struct {
			Name       string `json:"name"`
			Properties []struct {
				Name  string      `json:"name"`
				Value interface{} `json:"value"`
			} `json:"properties"`
		} `json:"objects"`
	} `json:"layers"`
}

type registerMsg struct {
	ExtensionID        string `json:"extension_id"`
	HeartbeatIntervalS int    `json:"heartbeat_interval_s"`
}

type triggerMsg struct {
	ExtensionID   string `json:"extension_id"`
	GateTriggers  []struct {
		ZoneID   string `json:"zone_id"`
		Behavior string `json:"behavior"`
	} `json:"gate_triggers"`
}

func main() {
	natsURL := envOr("NATS_URL", "nats://localhost:4222")
	pbURL := envOr("POCKETBASE_URL", "http://localhost:8090")
	mapID := envOr("MAP_ID", "test-map")
	extID := "walls"
	heartbeatS := 10

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	nc, err := nats.Connect(natsURL,
		nats.Name("ext-"+extID),
		nats.ReconnectWait(2*time.Second),
		nats.MaxReconnects(-1),
	)
	if err != nil {
		logger.Error("nats connect", "err", err)
		os.Exit(1)
	}
	defer nc.Close()

	regSubject := fmt.Sprintf("extension.%s.register", extID)
	trigSubject := fmt.Sprintf("extension.%s.register_triggers", extID)
	hbSubject := fmt.Sprintf("extension.%s.heartbeat", extID)

	// register reads wall zones from the map and publishes register + trigger
	// messages. Called at startup, on map.updated, and periodically.
	register := func() {
		zones, err := findWallZones(pbURL, mapID, logger)
		if err != nil {
			logger.Warn("find wall zones failed", "err", err)
			return
		}
		logger.Info("found wall zones", "count", len(zones), "zones", zones)

		var gateTriggers []struct {
			ZoneID   string `json:"zone_id"`
			Behavior string `json:"behavior"`
		}
		for _, zid := range zones {
			gateTriggers = append(gateTriggers, struct {
				ZoneID   string `json:"zone_id"`
				Behavior string `json:"behavior"`
			}{ZoneID: zid, Behavior: "block"})
		}

		regData, _ := json.Marshal(registerMsg{
			ExtensionID:        extID,
			HeartbeatIntervalS: heartbeatS,
		})
		trigData, _ := json.Marshal(triggerMsg{
			ExtensionID:  extID,
			GateTriggers: gateTriggers,
		})

		nc.Publish(regSubject, regData)
		nc.Publish(trigSubject, trigData)
		logger.Info("registered walls extension", "triggers", len(gateTriggers))
	}

	// Subscribe to map.updated so we re-read the map when it changes.
	if _, err := nc.Subscribe("map.updated", func(m *nats.Msg) {
		logger.Info("map.updated received, re-reading map", "map", string(m.Data))
		register()
	}); err != nil {
		logger.Error("subscribe map.updated", "err", err)
	}

	// worldsim.ready fires when worldsim's subscriptions are live (on startup
	// and on restart). Re-register whenever it fires so we never race the
	// initial publish.
	readyCh := make(chan struct{}, 1)
	nc.Subscribe("worldsim.ready", func(m *nats.Msg) {
		logger.Info("worldsim ready, registering", "map", string(m.Data))
		register()
		select {
		case readyCh <- struct{}{}:
		default:
		}
	})

	// Wait until PocketBase is up (register() reads the map from it).
	for i := 0; i < 30; i++ {
		zones, err := findWallZones(pbURL, mapID, logger)
		if err == nil {
			_ = zones // register() will re-fetch, but we just need to know PB is up
			break
		}
		logger.Warn("waiting for pocketbase", "attempt", i+1, "err", err)
		time.Sleep(time.Second)
	}

	// Wait for worldsim.ready before the initial registration. Fall back to
	// registering directly after a timeout (e.g. worldsim was already up and
	// we missed the broadcast on extension restart).
	select {
	case <-readyCh:
	case <-time.After(10 * time.Second):
		logger.Warn("worldsim.ready not received, registering anyway", "id", extID)
		register()
	}

	// Heartbeat + re-register loop.
	ticker := time.NewTicker(time.Duration(heartbeatS) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			logger.Info("shutting down")
			return
		case <-ticker.C:
			nc.Publish(hbSubject, []byte(extID))
			// Re-register every 3rd heartbeat.
			if time.Now().Unix()%int64(heartbeatS*3) < int64(heartbeatS) {
				register()
			}
		}
	}
}

// findWallZones reads the Tiled map from PocketBase and returns zone IDs
// that have zone_type "wall".
func findWallZones(pbURL, mapName string, logger *slog.Logger) ([]string, error) {
	pbURL = strings.TrimRight(pbURL, "/")

	// Fetch map record.
	resp, err := http.Get(fmt.Sprintf("%s/api/collections/maps/records?filter=(name=\"%s\")&perPage=1", pbURL, mapName))
	if err != nil {
		return nil, fmt.Errorf("fetch map record: %w", err)
	}
	defer resp.Body.Close()
	var record struct {
		Items []struct {
			ID           string `json:"id"`
			CollectionID string `json:"collectionId"`
			TiledJSON    string `json:"tiled_json"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&record); err != nil {
		return nil, fmt.Errorf("decode map record: %w", err)
	}
	if len(record.Items) == 0 {
		return nil, fmt.Errorf("no map found: %s", mapName)
	}

	r := record.Items[0]
	jsonURL := fmt.Sprintf("%s/api/files/%s/%s/%s", pbURL, r.CollectionID, r.ID, r.TiledJSON)

	// Fetch Tiled JSON.
	jresp, err := http.Get(jsonURL)
	if err != nil {
		return nil, fmt.Errorf("fetch tiled json: %w", err)
	}
	defer jresp.Body.Close()
	body, err := io.ReadAll(jresp.Body)
	if err != nil {
		return nil, fmt.Errorf("read tiled json: %w", err)
	}

	var tiled tiledMapJSON
	if err := json.Unmarshal(body, &tiled); err != nil {
		return nil, fmt.Errorf("parse tiled json: %w", err)
	}

	var wallZones []string
	for _, layer := range tiled.Layers {
		if strings.ToLower(layer.Name) != "zones" || layer.Type != "objectgroup" {
			continue
		}
		for _, obj := range layer.Objects {
			if obj.Name == "" {
				continue
			}
			for _, prop := range obj.Properties {
				if prop.Name == "zone_type" {
					if s, ok := prop.Value.(string); ok && s == "wall" {
						wallZones = append(wallZones, obj.Name)
					}
				}
			}
		}
	}
	return wallZones, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
