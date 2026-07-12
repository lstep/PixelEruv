package migrations

import (
	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

func init() {
	m.Register(func(app core.App) error {
		spriteBases, err := app.FindCollectionByNameOrId("sprite_bases")
		if err != nil {
			return err
		}

		// Find the char_4 / char_5 records and collect their IDs so we
		// can clear dangling player.sprite_base references before deleting.
		all, err := app.FindAllRecords(spriteBases)
		if err != nil {
			return err
		}
		var brokenIDs []string
		for _, r := range all {
			name := r.GetString("name")
			if name == "char_4" || name == "char_5" {
				brokenIDs = append(brokenIDs, r.Id)
			}
		}
		if len(brokenIDs) == 0 {
			return nil // idempotent: nothing to remove
		}

		// Clear sprite_base on any players referencing the broken records
		// so their avatars fall back to the hash-based static sheet instead
		// of pointing at a deleted texture.
		players, err := app.FindCollectionByNameOrId("players")
		if err != nil {
			return err
		}
		playerRecords, err := app.FindAllRecords(players)
		if err != nil {
			return err
		}
		brokenSet := make(map[string]bool, len(brokenIDs))
		for _, id := range brokenIDs {
			brokenSet[id] = true
		}
		for _, p := range playerRecords {
			if brokenSet[p.GetString("sprite_base")] {
				p.Set("sprite_base", "")
				if err := app.Save(p); err != nil {
					return err
				}
			}
		}

		// Delete the broken sprite_bases records.
		for _, r := range all {
			name := r.GetString("name")
			if name == "char_4" || name == "char_5" {
				if err := app.Delete(r); err != nil {
					return err
				}
			}
		}
		return nil
	}, func(app core.App) error {
		// No-op: the source PNGs were removed from the repo, so there is
		// nothing to restore.
		return nil
	})
}
