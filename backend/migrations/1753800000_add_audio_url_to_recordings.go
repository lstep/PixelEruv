package migrations

import (
	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

func init() {
	m.Register(func(app core.App) error {
		collection, err := app.FindCollectionByNameOrId("recordings")
		if err != nil {
			return err
		}
		// Idempotent: skip if the field already exists.
		if collection.Fields.GetByName("audio_url") != nil {
			return nil
		}
		collection.Fields.Add(
			&core.TextField{
				Name: "audio_url",
				Max:  500,
			},
		)
		return app.Save(collection)
	}, func(app core.App) error {
		collection, err := app.FindCollectionByNameOrId("recordings")
		if err != nil {
			return err
		}
		if collection.Fields.GetByName("audio_url") != nil {
			collection.Fields.RemoveByName("audio_url")
		}
		return app.Save(collection)
	})
}
