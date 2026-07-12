package migrations

import (
	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

func init() {
	m.Register(func(app core.App) error {
		// Add options JSON field to maps collection.
		mapsCol, err := app.FindCollectionByNameOrId("maps")
		if err != nil {
			return err
		}
		if mapsCol.Fields.GetByName("options") == nil {
			mapsCol.Fields.Add(&core.JSONField{
				Name: "options",
			})
			if err := app.Save(mapsCol); err != nil {
				return err
			}
		}

		// Add options JSON field to players collection.
		playersCol, err := app.FindCollectionByNameOrId("players")
		if err != nil {
			return err
		}
		if playersCol.Fields.GetByName("options") == nil {
			playersCol.Fields.Add(&core.JSONField{
				Name: "options",
			})
			if err := app.Save(playersCol); err != nil {
				return err
			}
		}

		return nil
	}, func(app core.App) error {
		mapsCol, err := app.FindCollectionByNameOrId("maps")
		if err == nil && mapsCol.Fields.GetByName("options") != nil {
			mapsCol.Fields.RemoveByName("options")
			app.Save(mapsCol)
		}
		playersCol, err := app.FindCollectionByNameOrId("players")
		if err == nil && playersCol.Fields.GetByName("options") != nil {
			playersCol.Fields.RemoveByName("options")
			app.Save(playersCol)
		}
		return nil
	})
}
