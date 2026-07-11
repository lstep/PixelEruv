package migrations

import (
	"os"

	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

func init() {
	m.Register(func(app core.App) error {
		superusers, err := app.FindCollectionByNameOrId(core.CollectionNameSuperusers)
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

		// Check if superuser already exists
		existing, _ := app.FindAuthRecordByEmail(core.CollectionNameSuperusers, email)
		if existing != nil {
			return nil
		}

		record := core.NewRecord(superusers)
		record.Set("email", email)
		record.Set("password", password)
		return app.Save(record)
	}, nil)
}
