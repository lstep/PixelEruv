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
		// Idempotent: skip fields that already exist.
		changed := false
		if collection.Fields.GetByName("audio_status") == nil {
			collection.Fields.Add(&core.TextField{
				Name: "audio_status",
				Max:  20,
			})
			changed = true
		}
		if collection.Fields.GetByName("audio_error") == nil {
			collection.Fields.Add(&core.TextField{
				Name: "audio_error",
				Max:  1000,
			})
			changed = true
		}
		if !changed {
			return nil
		}
		return app.Save(collection)
	}, func(app core.App) error {
		collection, err := app.FindCollectionByNameOrId("recordings")
		if err != nil {
			return err
		}
		for _, name := range []string{"audio_status", "audio_error"} {
			if collection.Fields.GetByName(name) != nil {
				collection.Fields.RemoveByName(name)
			}
		}
		return app.Save(collection)
	})
}
