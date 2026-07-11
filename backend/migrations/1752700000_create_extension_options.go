package migrations

import (
	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
	"github.com/pocketbase/pocketbase/tools/types"
)

func init() {
	m.Register(func(app core.App) error {
		// Idempotent: skip if the collection already exists.
		if existing, _ := app.FindCollectionByNameOrId("extension_options"); existing != nil {
			return nil
		}

		collection := core.NewBaseCollection("extension_options")

		collection.Fields.Add(
			&core.TextField{
				Name:     "extension_id",
				Required: true,
				Min:      1,
				Max:      100,
			},
			&core.JSONField{
				Name: "options",
			},
		)

		collection.ListRule = types.Pointer("")
		collection.ViewRule = types.Pointer("")
		collection.CreateRule = types.Pointer("")
		collection.UpdateRule = types.Pointer("")
		collection.DeleteRule = types.Pointer("")

		return app.Save(collection)
	}, func(app core.App) error {
		collection, err := app.FindCollectionByNameOrId("extension_options")
		if err != nil {
			return err
		}
		return app.Delete(collection)
	})
}
