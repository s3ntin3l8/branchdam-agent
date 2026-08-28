package luminar

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// createFixtureCatalog builds a catalog.db at dir/catalog.db on Luminar
// Neo's REAL schema (db_version 155), DDL copied verbatim from
// sqlite_master of a real catalog checked during issue #34's investigation
// -- not committed as a binary .db under testdata/, so the schema stays
// readable in source control and there's no stale binary to keep in sync
// with query.go's comments. See docs/luminar-catalog.md for the full
// verification record.
//
// Seeded rows cover every branch DefaultCatalogQuery/PairDerivatives has:
//   - a plain _upscale pair with matching camera+capture time (101 -> 102)
//   - a plain _panorama pair with matching camera but DIFFERENT capture time
//     (103 -> 104), the real-catalog case that proves captureTimeMatch must
//     stay evidence, never a pairing gate
//   - a trashed source paired with its edit (105, trash_bool=1 -> 106)
//   - an ambiguous stem: two images share "img_3000" (107, 108), so the
//     candidate (109) must NOT pair
//   - a derivative with no source image in the catalog at all (110)
//   - a pair on an unmounted volume (info_wide_ch = ”), so FullPath fails
//     for both sides (111 -> 112) -- also exercises the NULLIF/malformed-JSON
//     guard in DefaultCatalogQuery
func createFixtureCatalog(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "catalog.db")

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open fixture db: %v", err)
	}
	defer func() { _ = db.Close() }()

	const schema = `
CREATE TABLE volumes (_id_int_64 INTEGER NOT NULL, marked_to_delete_bool INTEGER NOT NULL DEFAULT 0, guid_wide_ch TEXT NOT NULL, info_wide_ch TEXT NOT NULL , PRIMARY KEY (_id_int_64) unique(guid_wide_ch,info_wide_ch));
CREATE TABLE paths (_id_int_64 INTEGER NOT NULL, marked_to_delete_bool INTEGER NOT NULL DEFAULT 0, volume_id_int_64 INTEGER NOT NULL, path_wide_ch TEXT NOT NULL , PRIMARY KEY (_id_int_64), FOREIGN KEY (volume_id_int_64) REFERENCES volumes (_id_int_64) unique(volume_id_int_64,path_wide_ch));
CREATE TABLE images (_id_int_64 INTEGER NOT NULL, marked_to_delete_bool INTEGER NOT NULL DEFAULT 0, guid_wide_ch TEXT NOT NULL UNIQUE, path_wide_ch TEXT NOT NULL, width INTEGER NOT NULL DEFAULT 0, height INTEGER NOT NULL DEFAULT 0, file_size_int_64 INTEGER NOT NULL DEFAULT 0, modification_date_int_64 INTEGER NOT NULL DEFAULT 0, import_date_int_64 INTEGER NOT NULL DEFAULT 0, import_index INTEGER NOT NULL DEFAULT 0, creation_date_int_64 INTEGER NOT NULL DEFAULT 0, file_type INTEGER NOT NULL DEFAULT 0, cloud_id_wide_ch TEXT NOT NULL DEFAULT '', updated_at_int_64 INTEGER NOT NULL DEFAULT 0, deleted_at_int_64 INTEGER NOT NULL DEFAULT 0, sync_status INTEGER NOT NULL DEFAULT 0 , PRIMARY KEY (_id_int_64));
CREATE TABLE image_user_attributes (_id_int_64 INTEGER NOT NULL, marked_to_delete_bool INTEGER NOT NULL DEFAULT 0, _out_id_int_64 INTEGER NOT NULL UNIQUE, trash_bool INTEGER NOT NULL DEFAULT 0, marker INTEGER NOT NULL DEFAULT 0, color INTEGER NOT NULL DEFAULT 0, rating INTEGER NOT NULL DEFAULT 0, edit_date_int_64 INTEGER NOT NULL DEFAULT 0, opened_bool INTEGER NOT NULL DEFAULT 0, origin_path_wide_ch TEXT NOT NULL DEFAULT '', _trashed_date_int_64 INTEGER NOT NULL DEFAULT 0, lost_bool INTEGER NOT NULL DEFAULT 0, tutorial INTEGER NOT NULL DEFAULT 0, smart_search_indexed INTEGER NOT NULL DEFAULT 0, virtual_copy_bool INTEGER NOT NULL DEFAULT 0, virtual_copy_date_int_64 INTEGER NOT NULL DEFAULT 0, virtual_copy_number INTEGER NOT NULL DEFAULT 0 , PRIMARY KEY (_id_int_64), FOREIGN KEY (_out_id_int_64) REFERENCES images (_id_int_64));
CREATE TABLE paths_images (_key_id_int_64 INTEGER NOT NULL, _val_id_int_64 INTEGER NOT NULL , PRIMARY KEY (_key_id_int_64,_val_id_int_64));
CREATE TABLE IF NOT EXISTS "image_exiv_attributes" (_id_int_64 INTEGER NOT NULL, marked_to_delete_bool INTEGER NOT NULL DEFAULT 0, _out_id_int_64 INTEGER NOT NULL UNIQUE, camera_model_wide_ch TEXT, date_time_int_64 INTEGER NOT NULL DEFAULT 0 , PRIMARY KEY (_id_int_64), FOREIGN KEY (_out_id_int_64) REFERENCES images (_id_int_64));
`
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("create fixture schema: %v", err)
	}

	stmts := []string{
		// volumes: 1 is unmounted (empty info_wide_ch, the real shape a real
		// catalog's own volume row 1 has), 2 has a real mount point.
		`INSERT INTO volumes (_id_int_64, guid_wide_ch, info_wide_ch) VALUES (1, '', '')`,
		`INSERT INTO volumes (_id_int_64, guid_wide_ch, info_wide_ch) VALUES (2, 'vol-2', '{"kMountPointSerializationKey":"/"}')`,

		`INSERT INTO paths (_id_int_64, volume_id_int_64, path_wide_ch) VALUES (10, 2, 'Users/test/Pictures')`,
		`INSERT INTO paths (_id_int_64, volume_id_int_64, path_wide_ch) VALUES (11, 1, 'Users/test/NoMount')`,

		// upscale pair, camera+time match
		`INSERT INTO images (_id_int_64, guid_wide_ch, path_wide_ch, creation_date_int_64) VALUES (101, 'g101', 'IMG_1000.jpeg', 100)`,
		`INSERT INTO images (_id_int_64, guid_wide_ch, path_wide_ch, creation_date_int_64) VALUES (102, 'g102', 'IMG_1000_upscale.jpg', 100)`,
		`INSERT INTO paths_images (_key_id_int_64, _val_id_int_64) VALUES (10, 101)`,
		`INSERT INTO paths_images (_key_id_int_64, _val_id_int_64) VALUES (10, 102)`,
		`INSERT INTO image_exiv_attributes (_id_int_64, _out_id_int_64, camera_model_wide_ch, date_time_int_64) VALUES (101, 101, 'Pixel 10', 100)`,
		`INSERT INTO image_exiv_attributes (_id_int_64, _out_id_int_64, camera_model_wide_ch, date_time_int_64) VALUES (102, 102, 'Pixel 10', 100)`,

		// panorama pair, camera matches but capture time deliberately doesn't
		`INSERT INTO images (_id_int_64, guid_wide_ch, path_wide_ch, creation_date_int_64) VALUES (103, 'g103', 'DJI_0002_D.JPG', 200)`,
		`INSERT INTO images (_id_int_64, guid_wide_ch, path_wide_ch, creation_date_int_64) VALUES (104, 'g104', 'DJI_0002_D_PANORAMA.tiff', 999)`,
		`INSERT INTO paths_images (_key_id_int_64, _val_id_int_64) VALUES (10, 103)`,
		`INSERT INTO paths_images (_key_id_int_64, _val_id_int_64) VALUES (10, 104)`,
		`INSERT INTO image_exiv_attributes (_id_int_64, _out_id_int_64, camera_model_wide_ch, date_time_int_64) VALUES (103, 103, 'FC9470', 200)`,
		`INSERT INTO image_exiv_attributes (_id_int_64, _out_id_int_64, camera_model_wide_ch, date_time_int_64) VALUES (104, 104, 'FC9470', 999)`,

		// trashed source, still pairs
		`INSERT INTO images (_id_int_64, guid_wide_ch, path_wide_ch, creation_date_int_64) VALUES (105, 'g105', 'IMG_2000.jpeg', 300)`,
		`INSERT INTO images (_id_int_64, guid_wide_ch, path_wide_ch, creation_date_int_64) VALUES (106, 'g106', 'IMG_2000_upscale.jpg', 300)`,
		`INSERT INTO paths_images (_key_id_int_64, _val_id_int_64) VALUES (10, 105)`,
		`INSERT INTO paths_images (_key_id_int_64, _val_id_int_64) VALUES (10, 106)`,
		`INSERT INTO image_user_attributes (_id_int_64, _out_id_int_64, trash_bool) VALUES (105, 105, 1)`,

		// ambiguous stem: two images share "img_3000"
		`INSERT INTO images (_id_int_64, guid_wide_ch, path_wide_ch, creation_date_int_64) VALUES (107, 'g107', 'IMG_3000.jpeg', 400)`,
		`INSERT INTO images (_id_int_64, guid_wide_ch, path_wide_ch, creation_date_int_64) VALUES (108, 'g108', 'IMG_3000.jpeg', 401)`,
		`INSERT INTO images (_id_int_64, guid_wide_ch, path_wide_ch, creation_date_int_64) VALUES (109, 'g109', 'IMG_3000_upscale.jpg', 400)`,
		`INSERT INTO paths_images (_key_id_int_64, _val_id_int_64) VALUES (10, 107)`,
		`INSERT INTO paths_images (_key_id_int_64, _val_id_int_64) VALUES (10, 108)`,
		`INSERT INTO paths_images (_key_id_int_64, _val_id_int_64) VALUES (10, 109)`,

		// no source in catalog for this derivative
		`INSERT INTO images (_id_int_64, guid_wide_ch, path_wide_ch, creation_date_int_64) VALUES (110, 'g110', 'IMG_4000_upscale.jpg', 500)`,
		`INSERT INTO paths_images (_key_id_int_64, _val_id_int_64) VALUES (10, 110)`,

		// pair on an unmounted volume -- FullPath fails for both sides
		`INSERT INTO images (_id_int_64, guid_wide_ch, path_wide_ch, creation_date_int_64) VALUES (111, 'g111', 'IMG_5000.jpeg', 600)`,
		`INSERT INTO images (_id_int_64, guid_wide_ch, path_wide_ch, creation_date_int_64) VALUES (112, 'g112', 'IMG_5000_upscale.jpg', 600)`,
		`INSERT INTO paths_images (_key_id_int_64, _val_id_int_64) VALUES (11, 111)`,
		`INSERT INTO paths_images (_key_id_int_64, _val_id_int_64) VALUES (11, 112)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("exec %q: %v", s, err)
		}
	}

	return path
}

func TestImages(t *testing.T) {
	dir := t.TempDir()
	path := createFixtureCatalog(t, dir)

	ctx := context.Background()
	cat, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = cat.Close() }()

	images, err := cat.Images(ctx, DefaultCatalogQuery)
	if err != nil {
		t.Fatalf("Images: %v", err)
	}
	if len(images) != 12 {
		t.Fatalf("expected 12 images, got %d: %+v", len(images), images)
	}

	byID := make(map[string]CatalogImage, len(images))
	for _, img := range images {
		byID[img.ImageID] = img
	}

	// image_id=101 sits on the mounted volume: FullPath must resolve and
	// join volume mount + dir_path + file_name correctly.
	img101, ok := byID["101"]
	if !ok {
		t.Fatal("image 101 not found")
	}
	if img101.FileName != "IMG_1000.jpeg" {
		t.Errorf("FileName = %q, want IMG_1000.jpeg", img101.FileName)
	}
	if img101.CameraModel != "Pixel 10" {
		t.Errorf("CameraModel = %q, want Pixel 10", img101.CameraModel)
	}
	if img101.CaptureTime != 100 {
		t.Errorf("CaptureTime = %d, want 100", img101.CaptureTime)
	}
	fullPath, ok := img101.FullPath()
	if !ok {
		t.Fatal("FullPath: expected ok=true for a mounted volume")
	}
	if want := "/Users/test/Pictures/IMG_1000.jpeg"; fullPath != want {
		t.Errorf("FullPath = %q, want %q", fullPath, want)
	}

	// image_id=105 has no exiv row and a trashed user-attributes row.
	img105, ok := byID["105"]
	if !ok {
		t.Fatal("image 105 not found")
	}
	if !img105.Trashed {
		t.Error("image 105 Trashed = false, want true")
	}

	// image_id=111 sits on the unmounted volume (empty info_wide_ch) --
	// FullPath must report ok=false, and the query itself must not have
	// errored on the empty JSON blob (the NULLIF/json_extract guard).
	img111, ok := byID["111"]
	if !ok {
		t.Fatal("image 111 not found")
	}
	if _, ok := img111.FullPath(); ok {
		t.Error("FullPath: expected ok=false for an unmounted volume")
	}
}

func TestImagesRejectsWrongColumnCount(t *testing.T) {
	dir := t.TempDir()
	path := createFixtureCatalog(t, dir)

	ctx := context.Background()
	cat, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = cat.Close() }()

	_, err = cat.Images(ctx, `SELECT path_wide_ch FROM images`)
	if err == nil {
		t.Fatal("expected an error for a query returning the wrong column count, got nil")
	}
}

func TestDumpSchema(t *testing.T) {
	dir := t.TempDir()
	path := createFixtureCatalog(t, dir)

	ctx := context.Background()
	cat, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = cat.Close() }()

	objs, err := cat.DumpSchema(ctx)
	if err != nil {
		t.Fatalf("DumpSchema: %v", err)
	}
	var sawImages, sawVolumes bool
	for _, o := range objs {
		if o.Type == "table" && o.Name == "images" {
			sawImages = true
		}
		if o.Type == "table" && o.Name == "volumes" {
			sawVolumes = true
		}
	}
	if !sawImages || !sawVolumes {
		t.Errorf("expected images and volumes tables in schema dump, got %+v", objs)
	}
}

func TestOpenRejectsPathWithQueryOrFragmentCharacters(t *testing.T) {
	ctx := context.Background()
	for _, bad := range []string{
		"/tmp/catalog.db?immutable=1",
		"/tmp/catalog.db#frag",
	} {
		if _, err := Open(ctx, bad); err == nil {
			t.Errorf("Open(%q): expected an error, got nil -- a '?'/'#' in the path can be misinterpreted as DSN query parameters", bad)
		}
	}
}

// TestImagesRejectsEmptyFileName covers the loud-failure branch for a query
// that returns a NULL/empty file_name -- almost certainly the wrong column
// against a changed schema. Surfacing this here, rather than silently
// mispairing images, is cheaper to debug than a bad edge discovered three
// layers up in the syncer.
func TestImagesRejectsEmptyFileName(t *testing.T) {
	dir := t.TempDir()
	path := createFixtureCatalog(t, dir)

	ctx := context.Background()
	cat, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = cat.Close() }()

	_, err = cat.Images(ctx, `SELECT CAST(_id_int_64 AS TEXT), '', '', '', 0, '', 0 FROM images LIMIT 1`)
	if err == nil {
		t.Fatal("expected an error for a query returning an empty file_name, got nil")
	}
}

// TestImagesQueryError covers db.QueryContext's own error branch -- a query
// referencing a table that doesn't exist in this catalog's schema (the most
// likely real-world cause: a query written against a Luminar Neo version
// whose schema has since changed).
func TestImagesQueryError(t *testing.T) {
	dir := t.TempDir()
	path := createFixtureCatalog(t, dir)

	ctx := context.Background()
	cat, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = cat.Close() }()

	_, err = cat.Images(ctx, `SELECT a, b, c, d, e, f, g FROM no_such_table`)
	if err == nil {
		t.Fatal("expected an error for a query against a nonexistent table, got nil")
	}
}

// TestDumpSchemaQueryUsesRealSQLiteMaster is a light sanity check that
// DumpSchema's SQL/type/name triples are non-degenerate for a CREATE
// TABLE statement, not just that a known table name appears (already
// covered by TestDumpSchema) -- confirms the SQL column itself is
// populated, which --dump-schema's whole value proposition depends on.
func TestDumpSchemaQueryUsesRealSQLiteMaster(t *testing.T) {
	dir := t.TempDir()
	path := createFixtureCatalog(t, dir)

	ctx := context.Background()
	cat, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = cat.Close() }()

	objs, err := cat.DumpSchema(ctx)
	if err != nil {
		t.Fatalf("DumpSchema: %v", err)
	}
	for _, o := range objs {
		if o.Type == "table" && o.Name == "images" {
			if o.SQL == "" {
				t.Error("images' SQL column is empty, want its CREATE TABLE statement")
			}
			return
		}
	}
	t.Fatal("images table not found in schema dump")
}

func TestOpenNonexistentFile(t *testing.T) {
	// mode=ro against a file that doesn't exist must fail, not silently
	// create one -- that's the whole point of read-only access to a
	// third-party app's live database.
	ctx := context.Background()
	_, err := Open(ctx, filepath.Join(t.TempDir(), "does-not-exist.db"))
	if err == nil {
		t.Fatal("expected an error opening a nonexistent catalog read-only, got nil")
	}
}
