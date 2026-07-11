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
		if collection.Fields.GetByName("is_admin") != nil {
			return nil
		}

		collection.Fields.Add(&core.BoolField{
			Name:     "is_admin",
			Required: false,
		})

		return app.Save(collection)
	}, func(app core.App) error {
		collection, err := app.FindCollectionByNameOrId("players")
		if err != nil {
			return err
		}

		collection.Fields.RemoveByName("is_admin")

		return app.Save(collection)
	})
}
