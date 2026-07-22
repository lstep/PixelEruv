package migrations

import (
	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
	"github.com/pocketbase/pocketbase/tools/types"
)

func init() {
	m.Register(func(app core.App) error {
		// Disable the public REST API for all server-side-only collections.
		// A nil rule means the action is not exposed via /api/collections/<name>.
		// worldsim owns all reads/writes via the in-process SDK (core.App),
		// which bypasses collection rules. The admin server uses a superadmin
		// token (admins also bypass rules). The frontend fetches map/sprite
		// assets via the custom /api/assets/ routes on worldsim's embedded PB
		// router, not via the collection REST API.
		//
		// bans, extension_options, and recordings were originally created with
		// types.Pointer("") (public) despite comments saying "server-side only"
		// — that was a bug: an empty-string rule means "always allow", not
		// "disabled". This migration fixes that by setting all rules to nil.
		//
		// maps and sprite_bases had public List/View rules so the frontend
		// could fetch records + files via /api/collections/ and /api/files/.
		// The frontend now uses /api/assets/ routes instead, so the collection
		// REST API can be fully locked down.
		collections := []string{
			"bans",
			"extension_options",
			"recordings",
			"maps",
			"sprite_bases",
		}
		for _, name := range collections {
			collection, err := app.FindCollectionByNameOrId(name)
			if err != nil {
				return err
			}
			collection.ListRule = nil
			collection.ViewRule = nil
			collection.CreateRule = nil
			collection.UpdateRule = nil
			collection.DeleteRule = nil
			if err := app.Save(collection); err != nil {
				return err
			}
		}
		return nil
	}, func(app core.App) error {
		// Revert each collection to its original rules from the create migrations.
		reverts := map[string]struct {
			list, view, create, update, delete *string
		}{
			// 1752900000_create_bans: all public (the buggy "server-side only").
			"bans": {
				types.Pointer(""), types.Pointer(""), types.Pointer(""), types.Pointer(""), types.Pointer(""),
			},
			// 1752700000_create_extension_options: all public.
			"extension_options": {
				types.Pointer(""), types.Pointer(""), types.Pointer(""), types.Pointer(""), types.Pointer(""),
			},
			// 1753700000_create_recordings: all public (the buggy "server-side only").
			"recordings": {
				types.Pointer(""), types.Pointer(""), types.Pointer(""), types.Pointer(""), types.Pointer(""),
			},
			// 1751700000_create_maps: List/View public, C/U/D nil (not set).
			"maps": {
				types.Pointer(""), types.Pointer(""), nil, nil, nil,
			},
			// 1751900000_create_sprite_bases: List/View public, C/U/D nil (not set).
			"sprite_bases": {
				types.Pointer(""), types.Pointer(""), nil, nil, nil,
			},
		}
		for name, r := range reverts {
			collection, err := app.FindCollectionByNameOrId(name)
			if err != nil {
				return err
			}
			collection.ListRule = r.list
			collection.ViewRule = r.view
			collection.CreateRule = r.create
			collection.UpdateRule = r.update
			collection.DeleteRule = r.delete
			if err := app.Save(collection); err != nil {
				return err
			}
		}
		return nil
	})
}
