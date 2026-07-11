package migrations

import (
	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
	"github.com/pocketbase/pocketbase/tools/types"
)

func init() {
	m.Register(func(app core.App) error {
		// Idempotent: skip if the collection already exists.
		if existing, _ := app.FindCollectionByNameOrId("bans"); existing != nil {
			return nil
		}

		collection := core.NewBaseCollection("bans")

		collection.Fields.Add(
			&core.TextField{
				Name:     "target_type",
				Required: true,
				Min:      1,
				Max:      20,
			},
			&core.TextField{
				Name:     "target_value",
				Required: true,
				Min:      1,
				Max:      200,
			},
			&core.TextField{
				Name:     "reason",
				Required: false,
				Max:      500,
			},
			&core.NumberField{
				Name:     "banned_until",
				Required: false,
			},
			&core.TextField{
				Name:     "banned_by",
				Required: false,
				Max:      200,
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
		collection, err := app.FindCollectionByNameOrId("bans")
		if err != nil {
			return err
		}
		return app.Delete(collection)
	})
}
