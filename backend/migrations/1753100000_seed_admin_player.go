package migrations

import (
	"os"

	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

func init() {
	m.Register(func(app core.App) error {
		// Create the admin user in the users auth collection.
		usersCollection, err := app.FindCollectionByNameOrId("users")
		if err != nil {
			return err
		}

		email := os.Getenv("PB_ADMIN_EMAIL")
		if email == "" {
			email = "admin@pixeleruv.local"
		}
		password := os.Getenv("PB_ADMIN_PASSWORD")
		if password == "" {
			password = "password123"
		}

		// Idempotent: skip if the user already exists.
		existingUser, _ := app.FindAuthRecordByEmail("users", email)
		if existingUser == nil {
			user := core.NewRecord(usersCollection)
			user.SetEmail(email)
			user.SetPassword(password)
			user.SetVerified(true)
			if err := app.Save(user); err != nil {
				return err
			}
			existingUser = user
		}

		// Create or update the admin player record, linked to the user.
		playersCollection, err := app.FindCollectionByNameOrId("players")
		if err != nil {
			return err
		}

		// Idempotent: skip if a player with this oidc_sub already exists.
		existing, _ := app.FindRecordsByFilter(
			playersCollection.Id,
			"oidc_sub = \""+existingUser.Id+"\"",
			"",
			1,
			0,
		)
		if len(existing) > 0 {
			if !existing[0].GetBool("is_admin") {
				existing[0].Set("is_admin", true)
				return app.Save(existing[0])
			}
			return nil
		}

		record := core.NewRecord(playersCollection)
		record.Set("oidc_sub", existingUser.Id)
		record.Set("entity_id", "e_admin")
		record.Set("display_name", "Admin")
		record.Set("is_admin", true)
		return app.Save(record)
	}, func(app core.App) error {
		playersCollection, err := app.FindCollectionByNameOrId("players")
		if err != nil {
			return err
		}
		records, _ := app.FindRecordsByFilter(
			playersCollection.Id,
			"is_admin = true",
			"",
			100,
			0,
		)
		for _, r := range records {
			_ = app.Delete(r)
		}
		return nil
	})
}
