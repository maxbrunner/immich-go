package archive

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/gdamore/tcell/v2"
	"github.com/simulot/immich-go/adapters"
	"github.com/simulot/immich-go/adapters/folder"
	"github.com/simulot/immich-go/internal/assettracker"
	"github.com/simulot/immich-go/internal/fileevent"
	"github.com/simulot/immich-go/internal/fileprocessor"
	"github.com/simulot/immich-go/internal/fshelper/osfs"
	"github.com/spf13/cobra"
)

func (ac *ArchiveCmd) Run(cmd *cobra.Command, adapter adapters.Reader) error {
	ctx := cmd.Context()
	log := ac.app.Log()
	log.Info("in ArchiveCmd.Run", "archivePath", ac.ArchivePath)

	// Initialize the Journal and FileProcessor
	if ac.app.FileProcessor() == nil {
		recorder := fileevent.NewRecorder(ac.app.Log().Logger)
		tracker := assettracker.NewWithLogger(ac.app.Log().Logger, ac.app.DryRun)
		processor := fileprocessor.New(tracker, recorder)
		ac.app.SetFileProcessor(processor)
	}

	p := ac.ArchivePath
	if err := os.MkdirAll(p, 0o755); err != nil {
		return err
	}

	destFS := osfs.DirFS(p)
	var err error
	ac.dest, err = folder.NewLocalAssetWriter(destFS, ".")
	if err != nil {
		return err
	}
	ac.indexCount, err = ac.dest.BuildIndex(ctx, log.Logger)
	if err != nil {
		return err
	}

	// Choose UI vs plain log
	runner := ac.runUIMode
	if ac.NoUI {
		runner = ac.runPlain
	} else if _, err := tcell.NewScreen(); err != nil {
		log.Warn("can't initialize screen, falling back to plain log", "err", err)
		fmt.Println("can't initialize screen, falling back to plain log")
		runner = ac.runPlain
	}

	return runner(ctx, adapter)
}

// runPlain runs the archive loop without a TUI (plain log output).
func (ac *ArchiveCmd) runPlain(ctx context.Context, adapter adapters.Reader) error {
	err := ac.browseAndArchive(ctx, adapter)
	ac.printSummary()
	return err
}

func (ac *ArchiveCmd) printSummary() {
	counts := ac.app.FileProcessor().Logger().GetCounts()
	fmt.Println()
	fmt.Println("=== Archive Summary ===")
	fmt.Printf("Local index:       %d\n", ac.indexCount)
	fmt.Printf("Discovered:        %d images, %d videos\n",
		counts[fileevent.DiscoveredImage], counts[fileevent.DiscoveredVideo])
	fmt.Printf("Downloaded (new):  %d\n", counts[fileevent.ProcessedFileArchived])
	fmt.Printf("Skipped:           %d\n", counts[fileevent.DiscardedLocalDuplicate])
	fmt.Printf("Metadata updated:  %d\n", counts[fileevent.ProcessedMetadataUpdated])
	fmt.Printf("Moved/renamed:     %d\n", counts[fileevent.ProcessedFileMoved])
	fmt.Printf("Errors:            %d\n", counts[fileevent.ErrorFileAccess])
	fmt.Println("=======================")
}

// browseAndArchive is the core processing loop: reads from adapter.Browse, writes each asset.
func (ac *ArchiveCmd) browseAndArchive(ctx context.Context, adapter adapters.Reader) error {
	log := ac.app.Log()
	gChan := adapter.Browse(ctx)
	errCount := 0
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case g, ok := <-gChan:
			if !ok {
				return nil
			}
			for _, a := range g.Assets {
				outcome, writeErr := ac.dest.WriteAsset(ctx, a)
				if writeErr == nil {
					writeErr = a.Close()
				}
				if writeErr != nil {
					ac.app.FileProcessor().RecordAssetError(ctx, a.File, int64(a.FileSize), fileevent.ErrorFileAccess, writeErr)
					errCount++
					if errCount > 5 {
						err := errors.New("too many errors, aborting")
						log.Error(err.Error())
						return err
					}
				} else {
					switch outcome {
					case folder.OutcomeDownloaded:
						ac.app.FileProcessor().RecordAssetProcessed(ctx, a.File, int64(a.FileSize), fileevent.ProcessedFileArchived)
						log.Info("downloaded", "file", a.File.Name())
					case folder.OutcomeSkipped:
						ac.app.FileProcessor().RecordAssetDiscarded(ctx, a.File, int64(a.FileSize), fileevent.DiscardedLocalDuplicate, "no change")
						log.Debug("skipped (no change)", "file", a.File.Name())
					case folder.OutcomeUpdated:
						ac.app.FileProcessor().RecordAssetProcessed(ctx, a.File, int64(a.FileSize), fileevent.ProcessedMetadataUpdated)
						log.Info("metadata updated", "file", a.File.Name())
					case folder.OutcomeMoved:
						ac.app.FileProcessor().RecordAssetProcessed(ctx, a.File, int64(a.FileSize), fileevent.ProcessedFileMoved)
						log.Info("moved/renamed", "file", a.File.Name())
					}
				}
			}
		}
	}
}
