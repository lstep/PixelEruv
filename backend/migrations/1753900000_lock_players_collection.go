package migrations

import (
	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
	"github.com/pocketbase/pocketbase/tools/types"
)

func init() {
	m.Register(func(app core.App) error {
		collection, err := app.FindCollectionByNameOrId("players")
		if err != nil {
			return err
		}

		// Disable the public REST API for the players collection entirely.
		// A nil rule means the action is not exposed via /api/collections/players.
		// worldsim owns all players reads/writes via the in-process SDK
		// (core.App), which bypasses collection rules, and the MCP server uses
		// an admin token (admins also bypass rules). The frontend never touches
		// the players REST API. This prevents anonymous callers from PATCHing
		// display_name, is_admin, entity_id, user_id, etc. directly via the
		// PocketBase HTTP API.
		collection.ListRule = nil
		collection.ViewRule = nil
		collection.CreateRule = nil
		collection.UpdateRule = nil
		collection.DeleteRule = nil

		return app.Save(collection)
	}, func(app core.App) error {
		collection, err := app.FindCollectionByNameOrId("players")
		if err != nil {
			return err
		}
		// Revert to the original public rules from 1751800000_create_players.
		collection.ListRule = types.Pointer("")
		collection.ViewRule = types.Pointer("")
		collection.CreateRule = types.Pointer("")
		collection.UpdateRule = types.Pointer("")
		collection.DeleteRule = types.Pointer("")
		return app.Save(collection)
	})
}
