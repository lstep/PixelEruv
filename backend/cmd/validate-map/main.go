// validate-map is a CLI tool that parses and validates a Tiled map JSON
// file, printing any integrity issues found. Exits non-zero if there are
// any ERROR-level issues. Map authors can run this before publishing a
// map to PocketBase to catch typos and structural problems early.
//
// Usage:
//
//	validate-map <path-to-tiled-json>
package main

import (
	"fmt"
	"os"

	"github.com/lstep/pixeleruv/backend/internal/worldsim"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintf(os.Stderr, "usage: %s <path-to-tiled-json>\n", os.Args[0])
		os.Exit(2)
	}
	path := os.Args[1]

	body, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read %s: %v\n", path, err)
		os.Exit(2)
	}

	md, err := worldsim.ParseTiledMapJSON(body)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse error: %v\n", err)
		os.Exit(1)
	}

	results := worldsim.CheckMapIntegrity(md)
	if len(results) == 0 {
		fmt.Printf("OK: %s is valid (%dx%d, %d entities, %d zones)\n",
			path, md.Width, md.Height, len(md.Entities), len(md.Zones))
		os.Exit(0)
	}

	errors, warnings, infos := 0, 0, 0
	for _, r := range results {
		switch r.Level {
		case worldsim.LevelError:
			errors++
			fmt.Printf("  ERROR   %s\n", r.String())
		case worldsim.LevelWarning:
			warnings++
			fmt.Printf("  WARN    %s\n", r.String())
		case worldsim.LevelInfo:
			infos++
			fmt.Printf("  INFO    %s\n", r.String())
		}
	}
	fmt.Printf("\n%d error(s), %d warning(s), %d info(s)\n", errors, warnings, infos)

	if errors > 0 {
		os.Exit(1)
	}
}
