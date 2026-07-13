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
		if collection.Fields.GetByName("hide_admin_badge") != nil {
			return nil
		}

		// hide_admin_badge lets an admin opt out of the public "admin" badge
		// on their name tag. Defaults to false (badge shown) — PB BoolField
		// zero value is false, so the default is "show badge" without any
		// backfill. Only meaningful when is_admin=true; ignored otherwise.
		collection.Fields.Add(&core.BoolField{
			Name:  "hide_admin_badge",
			Help:  "Hide the red \"admin\" badge on your name tag. Only takes effect when is_admin is true.",
		})

		return app.Save(collection)
	}, func(app core.App) error {
		collection, err := app.FindCollectionByNameOrId("players")
		if err != nil {
			return err
		}

		collection.Fields.RemoveByName("hide_admin_badge")

		return app.Save(collection)
	})
}
