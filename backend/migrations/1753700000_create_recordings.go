package migrations

import (
	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
	"github.com/pocketbase/pocketbase/tools/types"
)

func init() {
	m.Register(func(app core.App) error {
		// Idempotent: skip if the collection already exists.
		if existing, _ := app.FindCollectionByNameOrId("recordings"); existing != nil {
			return nil
		}

		collection := core.NewBaseCollection("recordings")

		collection.Fields.Add(
			&core.TextField{
				Name:     "meeting_id",
				Required: true,
				Min:      1,
				Max:      64,
			},
			&core.TextField{
				Name:     "room",
				Required: true,
				Min:      1,
				Max:      100,
			},
			&core.TextField{
				Name:     "zone_id",
				Required: false,
				Max:      100,
			},
			&core.TextField{
				Name:     "map_id",
				Required: false,
				Max:      100,
			},
			&core.TextField{
				Name:     "target",
				Required: true,
				Max:      20, // "mp4" | "youtube"
			},
			&core.TextField{
				Name:     "egress_id",
				Required: false,
				Max:      100,
			},
			&core.TextField{
				Name:     "started_by",
				Required: true,
				Max:      100, // entity_id of the admin host
			},
			&core.JSONField{
				Name:     "participants",
				Required: false,
			},
			&core.DateField{
				Name:     "start_time",
				Required: true,
			},
			&core.DateField{
				Name:     "end_time",
				Required: false,
			},
			&core.TextField{
				Name:     "status",
				Required: true,
				Max:      20, // "active" | "completed" | "error"
			},
			&core.TextField{
				Name:     "file_url",
				Required: false,
				Max:      500,
			},
			&core.JSONField{
				Name:     "consent_state",
				Required: false,
			},
		)

		// Server-side access only (Go SDK). No public API rules.
		collection.ListRule = types.Pointer("")
		collection.ViewRule = types.Pointer("")
		collection.CreateRule = types.Pointer("")
		collection.UpdateRule = types.Pointer("")
		collection.DeleteRule = types.Pointer("")

		return app.Save(collection)
	}, func(app core.App) error {
		collection, err := app.FindCollectionByNameOrId("recordings")
		if err != nil {
			return err
		}
		return app.Delete(collection)
	})
}
