package folder

import (
	"io/fs"
	"path"
	"testing"
	"testing/fstest"
	"time"

	"github.com/simulot/immich-go/internal/assets"
	"github.com/simulot/immich-go/internal/exif/sidecars/jsonsidecar"
	"github.com/simulot/immich-go/internal/fshelper"
	"github.com/simulot/immich-go/internal/fshelper/osfs"
)

const (
	contentA  = "content_a"
	checksumA = "Gql7vB/FAktegtXcDYLHwt4dfD8=" // base64 SHA1 of "content_a"
	contentB  = "content_b"
	checksumB = "VMiRD+59AzeUhR2QABoo7y7Cwe8=" // base64 SHA1 of "content_b"
)

var testDate = time.Date(2025, 1, 15, 0, 0, 0, 0, time.UTC)
var testDate2 = time.Date(2025, 3, 10, 0, 0, 0, 0, time.UTC)

func makeAsset(srcFS fs.FS, srcName, base, checksum string, captureDate time.Time) *assets.Asset {
	a := &assets.Asset{
		File:        fshelper.FSName(srcFS, srcName),
		Checksum:    checksum,
		CaptureDate: captureDate,
		FromApplication: &assets.Metadata{
			FileName:  base,
			Checksum:  checksum,
			DateTaken: captureDate,
		},
	}
	a.Base = base
	a.OriginalFileName = base
	return a
}

func newWriter(t *testing.T) (*LocalAssetWriter, fs.FS) {
	t.Helper()
	destFS := osfs.DirFS(t.TempDir())
	w, err := NewLocalAssetWriter(destFS, ".")
	if err != nil {
		t.Fatal(err)
	}
	if err := w.BuildIndex(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	return w, destFS
}

func readSidecar(t *testing.T, destFS fs.FS, jsonPath string) assets.Metadata {
	t.Helper()
	f, err := destFS.Open(jsonPath)
	if err != nil {
		t.Fatalf("open sidecar %s: %v", jsonPath, err)
	}
	defer f.Close()
	var md assets.Metadata
	if err := jsonsidecar.Read(f, &md); err != nil {
		t.Fatalf("read sidecar %s: %v", jsonPath, err)
	}
	return md
}

func TestWriteAsset_NewAsset(t *testing.T) {
	// Binary and JSON sidecar are created; sidecar has checksum and fileName.
	srcFS := fstest.MapFS{
		"asset-a": &fstest.MapFile{Data: []byte(contentA)},
	}
	w, destFS := newWriter(t)

	a := makeAsset(srcFS, "asset-a", "photo.jpg", checksumA, testDate)
	if err := w.WriteAsset(t.Context(), a); err != nil {
		t.Fatal(err)
	}

	dir := path.Join("2025", "2025-01")
	if _, err := fs.Stat(destFS, path.Join(dir, "photo.jpg")); err != nil {
		t.Errorf("binary not found: %v", err)
	}
	md := readSidecar(t, destFS, path.Join(dir, "photo.jpg.JSON"))
	if md.Checksum != checksumA {
		t.Errorf("expected checksum %q, got %q", checksumA, md.Checksum)
	}
	if md.FileName != "photo.jpg" {
		t.Errorf("expected fileName photo.jpg, got %q", md.FileName)
	}
}

func TestWriteAsset_IdempotentRerun(t *testing.T) {
	// Writing the same asset twice with identical metadata: binary not re-downloaded,
	// JSON sidecar NOT rewritten (mtime preserved for backup tools).
	srcFS := fstest.MapFS{
		"asset-a": &fstest.MapFile{Data: []byte(contentA)},
	}
	w, destFS := newWriter(t)
	ctx := t.Context()

	a1 := makeAsset(srcFS, "asset-a", "photo.jpg", checksumA, testDate)
	if err := w.WriteAsset(ctx, a1); err != nil {
		t.Fatalf("first write: %v", err)
	}

	dir := path.Join("2025", "2025-01")
	jsonPath := path.Join(dir, "photo.jpg.JSON")
	info1, _ := fs.Stat(destFS, jsonPath)

	// Build a fresh writer with BuildIndex to simulate a new run
	w2, _ := NewLocalAssetWriter(destFS, ".")
	if err := w2.BuildIndex(ctx, nil); err != nil {
		t.Fatal(err)
	}

	a2 := makeAsset(srcFS, "asset-a", "photo.jpg", checksumA, testDate)
	if err := w2.WriteAsset(ctx, a2); err != nil {
		t.Fatalf("second write: %v", err)
	}

	info2, _ := fs.Stat(destFS, jsonPath)
	if !info1.ModTime().Equal(info2.ModTime()) {
		t.Error("JSON mtime changed on idempotent rerun — sidecar was rewritten unnecessarily")
	}

	// Exactly one binary + one JSON
	entries, _ := fs.ReadDir(destFS, dir)
	if len(entries) != 2 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("expected 2 entries, got %d: %v", len(entries), names)
	}
}

func TestWriteAsset_MetadataUpdated(t *testing.T) {
	// Second write with a new album: JSON overwritten with updated metadata.
	srcFS := fstest.MapFS{
		"asset-a": &fstest.MapFile{Data: []byte(contentA)},
	}
	w, destFS := newWriter(t)
	ctx := t.Context()

	a1 := makeAsset(srcFS, "asset-a", "photo.jpg", checksumA, testDate)
	if err := w.WriteAsset(ctx, a1); err != nil {
		t.Fatal(err)
	}

	// Second run: asset now belongs to an album
	w2, _ := NewLocalAssetWriter(destFS, ".")
	if err := w2.BuildIndex(ctx, nil); err != nil {
		t.Fatal(err)
	}

	a2 := makeAsset(srcFS, "asset-a", "photo.jpg", checksumA, testDate)
	a2.FromApplication.Albums = []assets.Album{{Title: "Vacation"}}
	if err := w2.WriteAsset(ctx, a2); err != nil {
		t.Fatal(err)
	}

	md := readSidecar(t, destFS, path.Join("2025", "2025-01", "photo.jpg.JSON"))
	if len(md.Albums) != 1 || md.Albums[0].Title != "Vacation" {
		t.Errorf("expected album Vacation in sidecar, got %v", md.Albums)
	}
}

func TestWriteAsset_CollisionFallback(t *testing.T) {
	// No checksum → two different assets with the same filename get
	// photo.jpg and photo-1.jpg (dash suffix, not tilde).
	srcFS := fstest.MapFS{
		"asset-a": &fstest.MapFile{Data: []byte(contentA)},
		"asset-b": &fstest.MapFile{Data: []byte(contentB)},
	}
	w, destFS := newWriter(t)
	ctx := t.Context()

	aA := makeAsset(srcFS, "asset-a", "photo.jpg", "", testDate)
	aA.FromApplication.Checksum = ""
	aA.Checksum = ""
	aB := makeAsset(srcFS, "asset-b", "photo.jpg", "", testDate)
	aB.FromApplication.Checksum = ""
	aB.Checksum = ""

	if err := w.WriteAsset(ctx, aA); err != nil {
		t.Fatalf("write A: %v", err)
	}
	if err := w.WriteAsset(ctx, aB); err != nil {
		t.Fatalf("write B: %v", err)
	}

	dir := path.Join("2025", "2025-01")
	entries, _ := fs.ReadDir(destFS, dir)
	names := make(map[string]bool, len(entries))
	for _, e := range entries {
		names[e.Name()] = true
	}
	if !names["photo.jpg"] {
		t.Error("expected photo.jpg")
	}
	if !names["photo-1.jpg"] {
		t.Errorf("expected photo-1.jpg (dash suffix); files: %v", names)
	}
	if names["photo~1.jpg"] {
		t.Error("photo~1.jpg must not exist — tilde suffix is not used")
	}
}

func TestWriteAsset_DateFolderMove(t *testing.T) {
	// Second run with corrected CaptureDate: binary + JSON moved to new month folder.
	srcFS := fstest.MapFS{
		"asset-a": &fstest.MapFile{Data: []byte(contentA)},
	}
	w, destFS := newWriter(t)
	ctx := t.Context()

	a1 := makeAsset(srcFS, "asset-a", "photo.jpg", checksumA, testDate)
	if err := w.WriteAsset(ctx, a1); err != nil {
		t.Fatal(err)
	}

	oldDir := path.Join("2025", "2025-01")
	if _, err := fs.Stat(destFS, path.Join(oldDir, "photo.jpg")); err != nil {
		t.Fatalf("file not in expected initial location: %v", err)
	}

	// Second run: date corrected to March 2025
	w2, _ := NewLocalAssetWriter(destFS, ".")
	if err := w2.BuildIndex(ctx, nil); err != nil {
		t.Fatal(err)
	}

	a2 := makeAsset(srcFS, "asset-a", "photo.jpg", checksumA, testDate2)
	if err := w2.WriteAsset(ctx, a2); err != nil {
		t.Fatal(err)
	}

	newDir := path.Join("2025", "2025-03")
	if _, err := fs.Stat(destFS, path.Join(newDir, "photo.jpg")); err != nil {
		t.Errorf("file not moved to new dir: %v", err)
	}
	if _, err := fs.Stat(destFS, path.Join(newDir, "photo.jpg.JSON")); err != nil {
		t.Errorf("JSON not moved to new dir: %v", err)
	}
	// Old location must be gone
	if _, err := fs.Stat(destFS, path.Join(oldDir, "photo.jpg")); err == nil {
		t.Error("old binary still present after move")
	}
}

func TestWriteAsset_FilenameChange(t *testing.T) {
	// Same checksum, but FileName changed server-side: binary + JSON renamed.
	srcFS := fstest.MapFS{
		"asset-a": &fstest.MapFile{Data: []byte(contentA)},
	}
	w, destFS := newWriter(t)
	ctx := t.Context()

	a1 := makeAsset(srcFS, "asset-a", "photo.jpg", checksumA, testDate)
	if err := w.WriteAsset(ctx, a1); err != nil {
		t.Fatal(err)
	}

	w2, _ := NewLocalAssetWriter(destFS, ".")
	if err := w2.BuildIndex(ctx, nil); err != nil {
		t.Fatal(err)
	}

	// Server-side rename
	a2 := makeAsset(srcFS, "asset-a", "vacation.jpg", checksumA, testDate)
	if err := w2.WriteAsset(ctx, a2); err != nil {
		t.Fatal(err)
	}

	dir := path.Join("2025", "2025-01")
	if _, err := fs.Stat(destFS, path.Join(dir, "vacation.jpg")); err != nil {
		t.Errorf("renamed binary not found: %v", err)
	}
	if _, err := fs.Stat(destFS, path.Join(dir, "vacation.jpg.JSON")); err != nil {
		t.Errorf("renamed JSON not found: %v", err)
	}
	if _, err := fs.Stat(destFS, path.Join(dir, "photo.jpg")); err == nil {
		t.Error("old binary still present after rename")
	}
}

func TestUseMetadata_AppliesFileName(t *testing.T) {
	// UseMetadata must update OriginalFileName from md.FileName.
	a := &assets.Asset{OriginalFileName: "hash123.jpg"}
	md := &assets.Metadata{FileName: "original_photo.jpg"}
	a.UseMetadata(md)
	if a.OriginalFileName != "original_photo.jpg" {
		t.Errorf("expected OriginalFileName=original_photo.jpg, got %q", a.OriginalFileName)
	}
}
