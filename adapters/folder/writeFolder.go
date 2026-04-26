package folder

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"strings"

	"github.com/simulot/immich-go/internal/assets"
	"github.com/simulot/immich-go/internal/exif/sidecars/jsonsidecar"
	"github.com/simulot/immich-go/internal/fshelper"
	"github.com/simulot/immich-go/internal/fshelper/debugfiles"
)

// WriteOutcome describes the result of a WriteAsset call.
type WriteOutcome int

const (
	OutcomeDownloaded WriteOutcome = iota // path A: new file downloaded and saved
	OutcomeSkipped                        // path B: already on disk, metadata identical
	OutcomeUpdated                        // path B: already on disk, JSON sidecar refreshed
	OutcomeMoved                          // path B: already on disk, file moved or renamed (± metadata)
)

type closer interface {
	Close() error
}

type indexEntry struct {
	dir      string
	base     string          // full filename including extension, e.g. "photo.jpg"
	metadata assets.Metadata // loaded at index-build time for comparison
}

// LocalAssetWriter writes assets to a local directory tree organised by year/month.
// Call BuildIndex once before the first WriteAsset to enable idempotent reruns.
type LocalAssetWriter struct {
	WriteToFS  fs.FS
	createdDir map[string]struct{}
	index      map[string]indexEntry // checksum (base64) → on-disk location + metadata
	fileset    map[string]struct{}   // "dir/base" → present; O(1) collision detection
}

func NewLocalAssetWriter(fsys fs.FS, writeToPath string) (*LocalAssetWriter, error) {
	if _, ok := fsys.(fshelper.FSCanWrite); !ok {
		return nil, errors.New("FS does not support writing")
	}
	return &LocalAssetWriter{
		WriteToFS:  fsys,
		createdDir: make(map[string]struct{}),
		index:      make(map[string]indexEntry),
		fileset:    make(map[string]struct{}),
	}, nil
}

// BuildIndex scans all existing JSON sidecars in the destination FS and builds an
// in-memory index of checksum → file location. Must be called before WriteAsset.
// Sidecars without a Checksum field (written before this change) are skipped.
// Returns the number of indexed entries.
func (w *LocalAssetWriter) BuildIndex(ctx context.Context, log *slog.Logger) (int, error) {
	count := 0
	err := fs.WalkDir(w.WriteToFS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if !strings.EqualFold(path.Ext(p), ".json") {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		f, err := w.WriteToFS.Open(p)
		if err != nil {
			return nil // skip unreadable files
		}
		var md assets.Metadata
		readErr := jsonsidecar.Read(f, &md)
		f.Close()
		if readErr != nil || md.Checksum == "" {
			return nil // skip sidecars not written by archive (no checksum)
		}
		dir := path.Dir(p)
		base := strings.TrimSuffix(path.Base(p), path.Ext(p)) // strip ".JSON" / ".json"
		w.index[md.Checksum] = indexEntry{dir: dir, base: base, metadata: md}
		w.registerInFileset(dir, base)
		count++
		return nil
	})
	if log != nil && count > 0 {
		log.Info("archive index loaded", "entries", count)
	}
	return count, err
}

func (w *LocalAssetWriter) registerInFileset(dir, base string) {
	w.fileset[dir+"/"+base] = struct{}{}
	w.fileset[dir+"/"+base+".JSON"] = struct{}{}
}

func (w *LocalAssetWriter) deregisterFromFileset(dir, base string) {
	delete(w.fileset, dir+"/"+base)
	delete(w.fileset, dir+"/"+base+".JSON")
}

func (w *LocalAssetWriter) filesetContains(dir, base string) bool {
	_, ok := w.fileset[dir+"/"+base]
	return ok
}

func (w *LocalAssetWriter) WriteGroup(ctx context.Context, group *assets.Group) error {
	var err error
	if fsys, ok := w.WriteToFS.(closer); ok {
		defer fsys.Close()
	}
	for _, a := range group.Assets {
		select {
		case <-ctx.Done():
			return errors.Join(err, ctx.Err())
		default:
			_, writeErr := w.WriteAsset(ctx, a)
			err = errors.Join(err, writeErr)
		}
	}
	return err
}

func (w *LocalAssetWriter) WriteAsset(ctx context.Context, a *assets.Asset) (WriteOutcome, error) {
	dir := w.pathOfAsset(a)
	if err := w.ensureDir(dir); err != nil {
		return OutcomeDownloaded, err
	}

	select {
	case <-ctx.Done():
		return OutcomeDownloaded, ctx.Err()
	default:
	}

	incomingMd := w.buildMetadata(a)

	// Path B: asset already on disk (checksum found in index)
	if a.Checksum != "" {
		if entry, exists := w.index[a.Checksum]; exists {
			return w.updateExisting(ctx, dir, a, incomingMd, entry)
		}
	}

	// Path A: new asset
	return w.writeNew(ctx, dir, a, incomingMd)
}

// buildMetadata constructs the Metadata to write to the JSON sidecar from the asset.
func (w *LocalAssetWriter) buildMetadata(a *assets.Asset) assets.Metadata {
	var md assets.Metadata
	if a.FromApplication != nil {
		md = *a.FromApplication
	}
	md.Checksum = a.Checksum
	if md.FileName == "" {
		md.FileName = a.Base
	}
	return md
}

// writeNew downloads the asset and writes binary + JSON sidecar (path A).
func (w *LocalAssetWriter) writeNew(ctx context.Context, dir string, a *assets.Asset, incomingMd assets.Metadata) (WriteOutcome, error) {
	base := incomingMd.FileName
	if base == "" {
		base = a.Base
	}

	// Collision avoidance using O(1) fileset lookup
	ext := path.Ext(base)
	radical := base[:len(base)-len(ext)]
	for i := 1; w.filesetContains(dir, base) || w.filesetContains(dir, base+".JSON"); i++ {
		base = fmt.Sprintf("%s-%d%s", radical, i, ext)
	}

	r, err := a.OpenFile()
	if err != nil {
		return OutcomeDownloaded, err
	}
	defer r.Close()

	select {
	case <-ctx.Done():
		return OutcomeDownloaded, ctx.Err()
	default:
	}

	if err := fshelper.WriteFile(w.WriteToFS, path.Join(dir, base), r); err != nil {
		return OutcomeDownloaded, err
	}

	// XMP sidecar (copy from source if present)
	if a.FromSideCar != nil {
		scr, err := a.FromSideCar.File.Open()
		if err != nil {
			return OutcomeDownloaded, err
		}
		debugfiles.TrackOpenFile(scr, a.FromSideCar.File.Name())
		defer scr.Close()
		defer debugfiles.TrackCloseFile(scr)
		scw, err := fshelper.OpenFile(w.WriteToFS, path.Join(dir, base+".XMP"), os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			return OutcomeDownloaded, err
		}
		_, err = io.Copy(scw, scr)
		scw.Close()
		if err != nil {
			return OutcomeDownloaded, err
		}
	}

	if err := w.writeJSON(dir, base, &incomingMd); err != nil {
		return OutcomeDownloaded, err
	}

	// Register in index and fileset
	if incomingMd.Checksum != "" {
		w.index[incomingMd.Checksum] = indexEntry{dir: dir, base: base, metadata: incomingMd}
	}
	w.registerInFileset(dir, base)

	return OutcomeDownloaded, nil
}

// updateExisting handles an asset already present on disk (path B):
// moves/renames if location or filename changed, then refreshes the JSON sidecar
// if metadata changed. The binary is never re-downloaded.
func (w *LocalAssetWriter) updateExisting(_ context.Context, desiredDir string, a *assets.Asset, incomingMd assets.Metadata, entry indexEntry) (WriteOutcome, error) {
	desiredBase := incomingMd.FileName
	if desiredBase == "" {
		desiredBase = entry.base
	}

	// Resolve filename collision: if desiredBase is occupied by a *different* entry, use -N suffix.
	if desiredBase != entry.base || desiredDir != entry.dir {
		ext := path.Ext(desiredBase)
		radical := desiredBase[:len(desiredBase)-len(ext)]
		currentSlot := entry.dir + "/" + entry.base
		for i := 1; ; i++ {
			slot := desiredDir + "/" + desiredBase
			if !w.filesetContains(desiredDir, desiredBase) || slot == currentSlot {
				break
			}
			desiredBase = fmt.Sprintf("%s-%d%s", radical, i, ext)
		}
	}

	moved := false
	// Move/rename if location or name changed
	if desiredDir != entry.dir || desiredBase != entry.base {
		if err := w.ensureDir(desiredDir); err != nil {
			return OutcomeMoved, err
		}
		if err := w.moveFiles(entry.dir, entry.base, desiredDir, desiredBase); err != nil {
			return OutcomeMoved, err
		}
		w.deregisterFromFileset(entry.dir, entry.base)
		w.registerInFileset(desiredDir, desiredBase)
		entry = indexEntry{dir: desiredDir, base: desiredBase, metadata: entry.metadata}
		w.index[a.Checksum] = entry
		moved = true
	}

	// Refresh JSON sidecar only if metadata changed (preserves mtime for unchanged files)
	if !metadataEqual(entry.metadata, incomingMd) {
		if err := w.writeJSON(desiredDir, desiredBase, &incomingMd); err != nil {
			if moved {
				return OutcomeMoved, err
			}
			return OutcomeUpdated, err
		}
		w.index[a.Checksum] = indexEntry{dir: desiredDir, base: desiredBase, metadata: incomingMd}
		if moved {
			return OutcomeMoved, nil
		}
		return OutcomeUpdated, nil
	}

	if moved {
		return OutcomeMoved, nil
	}
	return OutcomeSkipped, nil
}

// moveFiles moves binary + JSON (and XMP if present) from old location to new.
func (w *LocalAssetWriter) moveFiles(oldDir, oldBase, newDir, newBase string) error {
	for _, suffix := range []string{"", ".JSON", ".XMP"} {
		oldP := path.Join(oldDir, oldBase+suffix)
		newP := path.Join(newDir, newBase+suffix)

		if suffix == ".XMP" {
			if _, err := fs.Stat(w.WriteToFS, oldP); err != nil {
				continue // XMP is optional
			}
		}

		if err := fshelper.Rename(w.WriteToFS, oldP, newP); err != nil {
			// Fallback: copy + delete
			if copyErr := w.copyAndDelete(oldP, newP); copyErr != nil {
				return copyErr
			}
		}
	}
	return nil
}

func (w *LocalAssetWriter) copyAndDelete(oldP, newP string) error {
	src, err := w.WriteToFS.Open(oldP)
	if err != nil {
		return err
	}
	dst, err := fshelper.OpenFile(w.WriteToFS, newP, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		src.Close()
		return err
	}
	_, copyErr := io.Copy(dst, src)
	dst.Close()
	src.Close()
	if copyErr != nil {
		return copyErr
	}
	return fshelper.Remove(w.WriteToFS, oldP)
}

func (w *LocalAssetWriter) writeJSON(dir, base string, md *assets.Metadata) error {
	scw, err := fshelper.OpenFile(w.WriteToFS, path.Join(dir, base+".JSON"), os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	err = jsonsidecar.Write(md, scw)
	scw.Close()
	return err
}

func (w *LocalAssetWriter) ensureDir(dir string) error {
	if _, ok := w.createdDir[dir]; ok {
		return nil
	}
	if err := fshelper.MkdirAll(w.WriteToFS, dir, 0o755); err != nil {
		return err
	}
	w.createdDir[dir] = struct{}{}
	return nil
}

// metadataEqual compares two Metadata structs field-by-field at the JSON level,
// ignoring the File field (not serialized) and the software version header.
func metadataEqual(a, b assets.Metadata) bool {
	a.File = fshelper.FSAndName{}
	b.File = fshelper.FSAndName{}
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return bytes.Equal(ab, bb)
}

func (w *LocalAssetWriter) pathOfAsset(a *assets.Asset) string {
	d := a.CaptureDate
	if d.IsZero() {
		return "no-date"
	}
	return path.Join(fmt.Sprintf("%04d", d.Year()), fmt.Sprintf("%04d-%02d", d.Year(), d.Month()))
}
