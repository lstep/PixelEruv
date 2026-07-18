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
		if collection.Fields.GetByName("thumbnail_url") == nil {
			collection.Fields.Add(&core.TextField{
				Name: "thumbnail_url",
				Max:  500,
			})
			return app.Save(collection)
		}
		return nil
	}, func(app core.App) error {
		collection, err := app.FindCollectionByNameOrId("recordings")
		if err != nil {
			return err
		}
		if collection.Fields.GetByName("thumbnail_url") != nil {
			collection.Fields.RemoveByName("thumbnail_url")
			return app.Save(collection)
		}
		return nil
	})
}
