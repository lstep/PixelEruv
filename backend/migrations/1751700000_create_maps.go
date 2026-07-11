package migrations

import (
	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
	"github.com/pocketbase/pocketbase/tools/types"
)

func init() {
	m.Register(func(app core.App) error {
		// Idempotent: skip if the collection already exists (e.g. from
		// a previous JS-migration-based PB instance reusing the same volume).
		if existing, _ := app.FindCollectionByNameOrId("maps"); existing != nil {
			return nil
		}

		collection := core.NewBaseCollection("maps")

		collection.Fields.Add(
			&core.TextField{
				Name:     "name",
				Required: true,
				Min:      1,
				Max:      100,
			},
			&core.FileField{
				Name:       "tiled_json",
				Required:   true,
				MaxSelect:  1,
				MaxSize:    5242880,
				MimeTypes:  []string{"application/json"},
			},
			&core.FileField{
				Name:       "tilesets",
				Required:   true,
				MaxSelect:  10,
				MaxSize:    10485760,
				MimeTypes:  []string{"image/png", "image/jpeg"},
			},
		)

		collection.ListRule = types.Pointer("")
		collection.ViewRule = types.Pointer("")

		return app.Save(collection)
	}, func(app core.App) error {
		collection, err := app.FindCollectionByNameOrId("maps")
		if err != nil {
			return err
		}
		return app.Delete(collection)
	})
}
