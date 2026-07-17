package migrations

import (
	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

func init() {
	m.Register(func(app core.App) error {
		collection, err := app.FindCollectionByNameOrId("players")
		if err != nil {
			return err
		}

		// Idempotent: skip if the field already exists.
		if collection.Fields.GetByName("status") != nil {
			return nil
		}

		// status persists the player's presence status across sessions so a
		// page reload restores the last value instead of resetting to
		// AVAILABLE. 0 = AVAILABLE, 1 = BUSY, 2 = DO_NOT_DISTURB. PB
		// NumberField zero value is 0 (AVAILABLE), so the default needs no
		// backfill. Guests have no PocketBase record and remain session-only.
		collection.Fields.Add(&core.NumberField{
			Name:     "status",
			Required: false,
		})

		return app.Save(collection)
	}, func(app core.App) error {
		collection, err := app.FindCollectionByNameOrId("players")
		if err != nil {
			return err
		}

		collection.Fields.RemoveByName("status")

		return app.Save(collection)
	})
}
