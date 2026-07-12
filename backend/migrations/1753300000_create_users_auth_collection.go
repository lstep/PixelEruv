package migrations

import (
	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
	"github.com/pocketbase/pocketbase/tools/types"
)

func init() {
	m.Register(func(app core.App) error {
		// Idempotent: skip if the collection already exists.
		if existing, _ := app.FindCollectionByNameOrId("users"); existing != nil {
			return nil
		}

		collection := core.NewAuthCollection("users")

		// Password auth with email as identity field.
		collection.PasswordAuth.Enabled = true
		collection.PasswordAuth.IdentityFields = []string{core.FieldNameEmail}

		// OAuth2 — enabled but providers are configured at runtime from env
		// vars (see worldsim main.go). The migration just enables the flag;
		// worldsim applies provider client IDs/secrets on startup.
		collection.OAuth2.Enabled = true

		// Require email verification before login.
		collection.AuthRule = types.Pointer("verified = true")

		// Public create API (self-registration). Users can view/update/delete
		// only their own record.
		ownerRule := "id = @request.auth.id"
		collection.ListRule = types.Pointer(ownerRule)
		collection.ViewRule = types.Pointer(ownerRule)
		collection.CreateRule = types.Pointer("")
		collection.UpdateRule = types.Pointer(ownerRule)
		collection.DeleteRule = types.Pointer(ownerRule)

		return app.Save(collection)
	}, func(app core.App) error {
		collection, err := app.FindCollectionByNameOrId("users")
		if err != nil {
			return err
		}
		return app.Delete(collection)
	})
}
