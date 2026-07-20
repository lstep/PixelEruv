// audit-assets is a CLI tool that scans a directory tree for PNG images and
// rejects any that exceed the WebGL maximum texture dimension or the file-size
// budget. Designed to run at `make sync-assets` time so oversized tilesets
// fail the build before they reach PocketBase or the browser.
//
// Defaults (configurable via env):
//
//	AUDIT_MAX_DIM    default 8192    — max pixels per side. 8192 is safe for
//	                                  all mobile + older iGPUs; WebGL2 spec
//	                                  minimum max is 16384 but many mobile
//	                                  GPUs cap at 8192.
//	AUDIT_MAX_BYTES  default 2097152 — max file size (2 MiB).
//
// Usage:
//
//	audit-assets <dir> [<dir>...]
//
// Exits 0 if every PNG is within budget, 1 if any violation is found, 2 on
// usage / IO errors.
package main

import (
	"fmt"
	"image/png"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "usage: %s <dir> [<dir>...]\n", os.Args[0])
		os.Exit(2)
	}

	maxDim := envInt("AUDIT_MAX_DIM", 8192)
	maxBytes := envInt("AUDIT_MAX_BYTES", 2*1024*1024)

	violations := 0
	for _, root := range os.Args[1:] {
		v := auditDir(root, maxDim, maxBytes)
		violations += v
	}

	if violations > 0 {
		fmt.Printf("\n%d image(s) exceeded the budget (max %dx%d, %d bytes). Fix or remove them before re-running.\n",
			violations, maxDim, maxDim, maxBytes)
		os.Exit(1)
	}
	fmt.Printf("OK: all images within budget (max %dx%d, %d bytes)\n", maxDim, maxDim, maxBytes)
}

// auditDir walks root and audits every .png. Returns the violation count.
func auditDir(root string, maxDim, maxBytes int) int {
	violations := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		ext := filepath.Ext(path)
		if ext != ".png" {
			return nil
		}

		info, err := os.Stat(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "stat %s: %v\n", path, err)
			violations++
			return nil
		}

		var w, h int
		f, err := os.Open(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "open %s: %v\n", path, err)
			violations++
			return nil
		}
		cfg, err := png.DecodeConfig(f)
		f.Close()
		if err != nil {
			fmt.Fprintf(os.Stderr, "decode %s: %v\n", path, err)
			violations++
			return nil
		}
		w, h = cfg.Width, cfg.Height

		var problems []string
		if w > maxDim || h > maxDim {
			problems = append(problems, fmt.Sprintf("dimensions %dx%d exceed max %d", w, h, maxDim))
		}
		if info.Size() > int64(maxBytes) {
			problems = append(problems, fmt.Sprintf("size %d bytes exceeds max %d", info.Size(), maxBytes))
		}
		if len(problems) > 0 {
			violations++
			fmt.Printf("FAIL  %s  (%s)\n", path, join(problems, "; "))
		}
		return nil
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "walk %s: %v\n", root, err)
		os.Exit(2)
	}
	return violations
}

func envInt(key string, def int) int {
	s := os.Getenv(key)
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

func join(ss []string, sep string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += sep
		}
		out += s
	}
	return out
}
