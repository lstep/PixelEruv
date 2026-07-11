package migrations

import (
	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
	"github.com/pocketbase/pocketbase/tools/types"
)

func init() {
	m.Register(func(app core.App) error {
		// Idempotent: skip if the collection already exists.
		if existing, _ := app.FindCollectionByNameOrId("players"); existing != nil {
			return nil
		}

		collection := core.NewBaseCollection("players")

		collection.Fields.Add(
			&core.TextField{
				Name:     "oidc_sub",
				Required: true,
				Min:      1,
				Max:      200,
			},
			&core.TextField{
				Name:     "entity_id",
				Required: true,
				Min:      1,
				Max:      100,
			},
			&core.TextField{
				Name:     "display_name",
				Required: false,
				Max:      100,
			},
			&core.NumberField{
				Name:     "pos_x",
				Required: false,
			},
			&core.NumberField{
				Name:     "pos_y",
				Required: false,
			},
		)

		collection.ListRule = types.Pointer("")
		collection.ViewRule = types.Pointer("")
		collection.CreateRule = types.Pointer("")
		collection.UpdateRule = types.Pointer("")
		collection.DeleteRule = types.Pointer("")

		return app.Save(collection)
	}, func(app core.App) error {
		collection, err := app.FindCollectionByNameOrId("players")
		if err != nil {
			return err
		}
		return app.Delete(collection)
	})
}
