// pb-collections exports and imports PocketBase collection schemas, records,
// and file fields between a PB data directory and a portable JSON + files
// directory. It works offline by bootstrapping PB directly on PB_DATA_DIR —
// the same pattern as seed-sprites. Do not run while worldsim is using the
// same data dir (SQLite is single-writer).
//
// System collections (_superusers, _externalAuths, _migrations) are skipped —
// only application collections are exported.
//
// Usage:
//
//	pb-collections -export <dir>          export all app collections to <dir>
//	pb-collections -import <dir>          import from <dir> into PB_DATA_DIR
//	pb-collections -export <dir> -force   overwrite a non-empty export dir
//	pb-collections -import <dir> -force   delete existing records before import
//
// Env:
//
//	PB_DATA_DIR    (default ./pb_data) — must match worldsim's data dir
//
// Export layout:
//
//	<dir>/collections.json
//	<dir>/files/<collectionName>/<recordId>/<filename>
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/filesystem"

	// Register Go migrations (side-effect import) so bootstrap creates the
	// initial schema on a fresh target data dir.
	_ "github.com/lstep/pixeleruv/backend/migrations"
)

const exportVersion = 1

type exportDoc struct {
	Version     int                `json:"version"`
	ExportedAt  string             `json:"exported_at"`
	Collections []exportCollection `json:"collections"`
}

type exportCollection struct {
	Name    string            `json:"name"`
	Schema  json.RawMessage   `json:"schema"`
	Records []json.RawMessage `json:"records"`
}

func main() {
	exportDir := flag.String("export", "", "export all app collections into this directory")
	importDir := flag.String("import", "", "import collections from this directory into PB_DATA_DIR")
	force := flag.Bool("force", false, "overwrite a non-empty export dir, or delete existing records before import")
	flag.Parse()

	if (*exportDir == "") == (*importDir == "") {
		fmt.Fprintln(os.Stderr, "pb-collections: specify exactly one of -export or -import")
		os.Exit(2)
	}

	pbDataDir := envOr("PB_DATA_DIR", "./pb_data")
	app := pocketbase.NewWithConfig(pocketbase.Config{DefaultDataDir: pbDataDir})
	if err := app.Bootstrap(); err != nil {
		fmt.Fprintf(os.Stderr, "pb-collections: pocketbase bootstrap: %v\n", err)
		os.Exit(1)
	}
	if err := app.RunAllMigrations(); err != nil {
		fmt.Fprintf(os.Stderr, "pb-collections: pocketbase migrations: %v\n", err)
		os.Exit(1)
	}

	switch {
	case *exportDir != "":
		if err := runExport(app, *exportDir, *force); err != nil {
			fmt.Fprintf(os.Stderr, "pb-collections: export: %v\n", err)
			os.Exit(1)
		}
	case *importDir != "":
		if err := runImport(app, *importDir, *force); err != nil {
			fmt.Fprintf(os.Stderr, "pb-collections: import: %v\n", err)
			os.Exit(1)
		}
	}
}

func runExport(app core.App, dir string, force bool) error {
	if empty, err := dirIsEmpty(dir); err != nil {
		return err
	} else if !empty && !force {
		return fmt.Errorf("export dir %q is not empty (use -force to overwrite)", dir)
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("clear export dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "files"), 0o755); err != nil {
		return fmt.Errorf("create export dir: %w", err)
	}

	fsys, err := app.NewFilesystem()
	if err != nil {
		return fmt.Errorf("init filesystem: %w", err)
	}
	defer fsys.Close()

	collections, err := app.FindAllCollections()
	if err != nil {
		return fmt.Errorf("list collections: %w", err)
	}

	doc := exportDoc{Version: exportVersion, ExportedAt: time.Now().UTC().Format(time.RFC3339)}
	for _, c := range collections {
		if c.System {
			continue
		}
		ec, err := exportCollectionData(app, c, fsys, dir)
		if err != nil {
			return fmt.Errorf("collection %q: %w", c.Name, err)
		}
		doc.Collections = append(doc.Collections, ec)
		log.Printf("export: %-20s %d records", c.Name, len(ec.Records))
	}

	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal export doc: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "collections.json"), raw, 0o644); err != nil {
		return fmt.Errorf("write collections.json: %w", err)
	}
	log.Printf("export done: %s (%d collections)", dir, len(doc.Collections))
	return nil
}

func exportCollectionData(app core.App, c *core.Collection, fsys *filesystem.System, dir string) (exportCollection, error) {
	schema, err := json.Marshal(c)
	if err != nil {
		return exportCollection{}, fmt.Errorf("marshal schema: %w", err)
	}

	records, err := app.FindAllRecords(c)
	if err != nil {
		return exportCollection{}, fmt.Errorf("list records: %w", err)
	}

	fileFields := fileFieldNames(c)
	ec := exportCollection{Name: c.Name, Schema: schema}
	for _, r := range records {
		// Copy file field binaries into <dir>/files/<collection>/<recordId>/<filename>.
		for _, fn := range fileFields {
			for _, name := range r.GetStringSlice(fn) {
				if name == "" {
					continue
				}
				key := r.BaseFilesPath() + "/" + name
				reader, err := fsys.GetReader(key)
				if err != nil {
					return exportCollection{}, fmt.Errorf("read file %s: %w", key, err)
				}
				dst := filepath.Join(dir, "files", c.Name, r.Id, name)
				if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
					reader.Close()
					return exportCollection{}, err
				}
				if err := copyReaderToFile(reader, dst); err != nil {
					reader.Close()
					return exportCollection{}, fmt.Errorf("write %s: %w", dst, err)
				}
				reader.Close()
			}
		}
		raw, err := json.Marshal(r)
		if err != nil {
			return exportCollection{}, fmt.Errorf("marshal record %s: %w", r.Id, err)
		}
		ec.Records = append(ec.Records, raw)
	}
	return ec, nil
}

func runImport(app core.App, dir string, force bool) error {
	raw, err := os.ReadFile(filepath.Join(dir, "collections.json"))
	if err != nil {
		return fmt.Errorf("read collections.json: %w", err)
	}
	var doc exportDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("parse collections.json: %w", err)
	}
	if doc.Version != exportVersion {
		return fmt.Errorf("unsupported export version %d (expected %d)", doc.Version, exportVersion)
	}

	// Upsert collection schemas to match the export (deleteMissing=false so
	// unrelated collections are left intact).
	schemas := make([]json.RawMessage, 0, len(doc.Collections))
	for _, ec := range doc.Collections {
		schemas = append(schemas, ec.Schema)
	}
	schemaJSON, err := json.Marshal(schemas)
	if err != nil {
		return fmt.Errorf("marshal schema array: %w", err)
	}
	if err := app.ImportCollectionsByMarshaledJSON(schemaJSON, false); err != nil {
		return fmt.Errorf("import schemas: %w", err)
	}

	filesRoot := filepath.Join(dir, "files")
	for _, ec := range doc.Collections {
		if err := importCollectionRecords(app, ec, filesRoot, force); err != nil {
			return fmt.Errorf("import collection %q: %w", ec.Name, err)
		}
	}
	log.Printf("import done: %s (%d collections)", dir, len(doc.Collections))
	return nil
}

func importCollectionRecords(app core.App, ec exportCollection, filesRoot string, force bool) error {
	collection, err := app.FindCollectionByNameOrId(ec.Name)
	if err != nil {
		return fmt.Errorf("find collection: %w", err)
	}

	if force {
		existing, err := app.FindAllRecords(collection)
		if err != nil {
			return fmt.Errorf("list existing records: %w", err)
		}
		for _, r := range existing {
			if err := app.Delete(r); err != nil {
				return fmt.Errorf("delete record %s: %w", r.Id, err)
			}
		}
	}

	fileFields := fileFieldNames(collection)
	imported := 0
	for _, raw := range ec.Records {
		var data map[string]any
		if err := json.Unmarshal(raw, &data); err != nil {
			return fmt.Errorf("unmarshal record: %w", err)
		}
		id := fmt.Sprintf("%v", data["id"])

		// Idempotent: skip records that already exist (unless -force cleared them).
		// FindRecordById returns a non-nil error when the record is absent (the
		// expected case here), but also on real failures (DB issue, missing
		// collection). Log the latter so they're not silently swallowed.
		existing, findErr := app.FindRecordById(ec.Name, id)
		if findErr != nil {
			log.Printf("import: lookup %s/%s: %v (treating as not found)", ec.Name, id, findErr)
		}
		if existing != nil {
			continue
		}

		record := core.NewRecord(collection)
		if err := json.Unmarshal(raw, record); err != nil {
			return fmt.Errorf("load record %s: %w", id, err)
		}
		// Preserve the original record id for idempotency (so re-imports skip
		// existing records). In -force mode the old records (and their storage
		// dirs) were just deleted, so reusing the same id would make PB's file
		// upload race on the removed directory — let PB mint a fresh id instead.
		if !force {
			record.Id = id
		}

		// Re-attach file fields from the export files dir. The exported JSON
		// stores filenames as strings, but PB rejects plain-string filenames
		// on create — new files must be *filesystem.File values.
		for _, fn := range fileFields {
			names := toStringSlice(data[fn])
			if len(names) == 0 {
				continue
			}
			files := make([]*filesystem.File, 0, len(names))
			for _, name := range names {
				// Export files are keyed by the ORIGINAL record id, regardless
				// of whether we preserve the id on import.
				p := filepath.Join(filesRoot, ec.Name, id, name)
				b, err := os.ReadFile(p)
				if err != nil {
					return fmt.Errorf("read export file %s: %w", p, err)
				}
				f, err := filesystem.NewFileFromBytes(b, name)
				if err != nil {
					return fmt.Errorf("create file %s: %w", name, err)
				}
				files = append(files, f)
			}
			if field := collection.Fields.GetByName(fn); field != nil {
				if ff, ok := field.(*core.FileField); ok && ff.IsMultiple() {
					record.Set(fn, files)
				} else {
					record.Set(fn, files[0])
				}
			}
		}

		// SaveNoValidate trusts the export as-is. Plain app.Save would reject
		// non-standard record ids (PB enforces a 15-char minimum) and re-run
		// field validations that the source DB already satisfied. The file
		// upload interceptors still run under SaveNoValidate.
		if err := app.SaveNoValidate(record); err != nil {
			return fmt.Errorf("save record %s: %w", id, err)
		}
		imported++
	}
	log.Printf("import: %-20s %d records", ec.Name, imported)
	return nil
}

// fileFieldNames returns the names of all "file" type fields on a collection.
func fileFieldNames(c *core.Collection) []string {
	var names []string
	for _, f := range c.Fields {
		if f.Type() == core.FieldTypeFile {
			names = append(names, f.GetName())
		}
	}
	return names
}

func toStringSlice(v any) []string {
	switch val := v.(type) {
	case []string:
		return val
	case []any:
		out := make([]string, 0, len(val))
		for _, item := range val {
			if s := fmt.Sprintf("%v", item); s != "" {
				out = append(out, s)
			}
		}
		return out
	case string:
		if val == "" {
			return nil
		}
		return []string{val}
	default:
		return nil
	}
}

func copyReaderToFile(r io.Reader, dst string) error {
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, r)
	return err
}

func dirIsEmpty(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return true, nil
		}
		return false, err
	}
	return len(entries) == 0, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
