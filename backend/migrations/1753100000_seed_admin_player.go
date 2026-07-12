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

		// Idempotent: skip if a player with oidc_sub="admin" already exists.
		existing, _ := app.FindRecordsByFilter(
			collection.Id,
			"oidc_sub = \"admin\"",
			"",
			1,
			0,
		)
		if len(existing) > 0 {
			// Ensure is_admin is set even if the record already exists
			// (e.g. the admin played before this migration ran).
			if !existing[0].GetBool("is_admin") {
				existing[0].Set("is_admin", true)
				return app.Save(existing[0])
			}
			return nil
		}

		record := core.NewRecord(collection)
		// Dex encodes the userID as a protobuf payload, so the oidc_sub
		// in JWT tokens is the base64 of that payload, not the raw userID.
		// For userID "admin" (local connector), Dex produces this sub:
		record.Set("oidc_sub", "CgVhZG1pbhIFbG9jYWw")
		record.Set("entity_id", "e_admin")
		record.Set("display_name", "Admin")
		record.Set("is_admin", true)
		return app.Save(record)
	}, func(app core.App) error {
		collection, err := app.FindCollectionByNameOrId("players")
		if err != nil {
			return err
		}
		records, _ := app.FindRecordsByFilter(
			collection.Id,
			"oidc_sub = \"admin\"",
			"",
			1,
			0,
		)
		for _, r := range records {
			_ = app.Delete(r)
		}
		return nil
	})
}
