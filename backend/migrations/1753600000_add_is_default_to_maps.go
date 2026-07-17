package migrations

import (
	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

func init() {
	m.Register(func(app core.App) error {
		collection, err := app.FindCollectionByNameOrId("maps")
		if err != nil {
			return err
		}

		// Idempotent: skip if the field already exists.
		if collection.Fields.GetByName("is_default") != nil {
			return nil
		}

		// is_default marks the map where new players spawn. Exactly one map
		// should have this set; worldsim fails fast at startup if no map has
		// it. The BoolField zero value is false, so existing records start
		// unset — the backfill below marks one so existing deployments keep
		// working without manual action.
		collection.Fields.Add(&core.BoolField{
			Name: "is_default",
			Help: "Mark this as the map where new players spawn. Set on exactly one map.",
		})
		if err := app.Save(collection); err != nil {
			return err
		}

		// Backfill: if no existing record has is_default=true, mark the map
		// named "main" (or, if no "main", the first record) as default.
		records, err := app.FindAllRecords(collection)
		if err != nil {
			return err
		}
		anyDefault := false
		for _, r := range records {
			if r.GetBool("is_default") {
				anyDefault = true
				break
			}
		}
		if anyDefault {
			return nil
		}
		if len(records) == 0 {
			return nil
		}
		target := -1
		for i, r := range records {
			if r.GetString("name") == "main" {
				target = i
				break
			}
		}
		if target == -1 {
			target = 0
		}
		records[target].Set("is_default", true)
		return app.Save(records[target])
	}, func(app core.App) error {
		collection, err := app.FindCollectionByNameOrId("maps")
		if err != nil {
			return err
		}
		collection.Fields.RemoveByName("is_default")
		return app.Save(collection)
	})
}
