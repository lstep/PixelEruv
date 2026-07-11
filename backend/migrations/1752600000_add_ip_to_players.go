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
		if collection.Fields.GetByName("ip") != nil {
			return nil
		}

		collection.Fields.Add(
			&core.TextField{
				Name:     "ip",
				Required: false,
				Max:      64,
			},
			&core.NumberField{
				Name:     "last_seen_at",
				Required: false,
			},
		)

		return app.Save(collection)
	}, func(app core.App) error {
		collection, err := app.FindCollectionByNameOrId("players")
		if err != nil {
			return err
		}

		collection.Fields.RemoveByName("ip")
		collection.Fields.RemoveByName("last_seen_at")

		return app.Save(collection)
	})
}
