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
		if collection.Fields.GetByName("sprite_base") != nil {
			return nil
		}

		collection.Fields.Add(&core.TextField{
			Name:     "sprite_base",
			Required: false,
		})

		return app.Save(collection)
	}, func(app core.App) error {
		collection, err := app.FindCollectionByNameOrId("players")
		if err != nil {
			return err
		}

		collection.Fields.RemoveByName("sprite_base")

		return app.Save(collection)
	})
}
