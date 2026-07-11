package migrations

import (
	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
	"github.com/pocketbase/pocketbase/tools/types"
)

func init() {
	m.Register(func(app core.App) error {
		// Idempotent: skip if the collection already exists.
		if existing, _ := app.FindCollectionByNameOrId("sprite_bases"); existing != nil {
			return nil
		}

		collection := core.NewBaseCollection("sprite_bases")

		collection.Fields.Add(
			&core.TextField{
				Name:     "name",
				Required: true,
				Min:      1,
				Max:      100,
			},
			&core.FileField{
				Name:       "sheet",
				Required:   true,
				MaxSelect:  1,
				MaxSize:    1048576,
				MimeTypes:  []string{"image/png"},
			},
		)

		collection.ListRule = types.Pointer("")
		collection.ViewRule = types.Pointer("")

		return app.Save(collection)
	}, func(app core.App) error {
		collection, err := app.FindCollectionByNameOrId("sprite_bases")
		if err != nil {
			return err
		}
		return app.Delete(collection)
	})
}
