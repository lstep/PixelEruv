package migrations

import (
	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

func init() {
	m.Register(func(app core.App) error {
		// Drop the worlds collection (removed — one world, multiple maps).
		if worlds, _ := app.FindCollectionByNameOrId("worlds"); worlds != nil {
			if err := app.Delete(worlds); err != nil {
				// Non-fatal: collection may have records that prevent deletion.
				// The collection is harmless if it lingers.
			}
		}

		// Remove world_id field from maps if it exists.
		if maps, _ := app.FindCollectionByNameOrId("maps"); maps != nil {
			if maps.Fields.GetByName("world_id") != nil {
				maps.Fields.RemoveByName("world_id")
				_ = app.Save(maps)
			}
		}

		return nil
	}, nil)
}
