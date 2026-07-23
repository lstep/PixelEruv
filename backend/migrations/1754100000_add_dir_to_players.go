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
		if collection.Fields.GetByName("dir") != nil {
			return nil
		}

		// dir persists the player's last facing direction across sessions so
		// a respawn restores it instead of always facing down. Matches the
		// Position component dir field: 0=down, 1=left, 2=right, 3=up. PB
		// NumberField zero value is 0 (down), so the default needs no
		// backfill. Guests have no PocketBase record and remain session-only.
		collection.Fields.Add(&core.NumberField{
			Name:     "dir",
			Required: false,
		})

		return app.Save(collection)
	}, func(app core.App) error {
		collection, err := app.FindCollectionByNameOrId("players")
		if err != nil {
			return err
		}

		collection.Fields.RemoveByName("dir")

		return app.Save(collection)
	})
}
