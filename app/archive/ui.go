package archive

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
	"github.com/simulot/immich-go/adapters"
	"github.com/simulot/immich-go/app"
	"github.com/simulot/immich-go/internal/assettracker"
	"github.com/simulot/immich-go/internal/fileevent"
	"github.com/simulot/immich-go/internal/fileprocessor"
	"golang.org/x/sync/errgroup"
)

type archiveUI struct {
	screen        *tview.Grid
	discoveryZone *tview.Grid
	resultsZone   *tview.Grid
	logView       *tview.TextView

	counts     map[fileevent.Code]*tview.TextView
	extraViews map[string]*tview.TextView

	tracker *assettracker.AssetTracker
	fp      *fileprocessor.FileProcessor
}

// runUIMode launches the tview-based archive UI and runs browseAndArchive in a goroutine.
func (ac *ArchiveCmd) runUIMode(ctx context.Context, adapter adapters.Reader) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	uiApp := tview.NewApplication()
	ui := ac.newArchiveUI(ac.app)

	pages := tview.NewPages()
	pages.AddPage("ui", ui.screen, true, true)
	uiApp.SetRoot(pages, true)

	var archiveDone atomic.Bool
	var messages strings.Builder

	stopUI := func(err error) {
		cancel(err)
		uiApp.Stop()
	}

	uiApp.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyCtrlQ, tcell.KeyCtrlC:
			ui.restoreLogger(ac.app)
			cancel(errors.New("interrupted: Ctrl+C or Ctrl+Q pressed"))
		case tcell.KeyEnter:
			if archiveDone.Load() {
				stopUI(nil)
			}
		}
		return event
	})

	// 100 ms tick to refresh counters
	go func() {
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				uiApp.QueueUpdateDraw(func() {
					ui.refresh(ac.app)
				})
			}
		}
	}()

	var g errgroup.Group

	// tview event loop
	g.Go(func() error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			err := uiApp.Run()
			cancel(err)
			return err
		}
	})

	// archive processing
	g.Go(func() error {
		err := ac.browseAndArchive(ctx, adapter)

		counts := ac.app.FileProcessor().Logger().GetCounts()
		if counts[fileevent.ErrorFileAccess] > 0 {
			messages.WriteString("Some errors occurred. Check the log file for details.\n")
		}

		archiveDone.Store(true)
		uiApp.QueueUpdateDraw(func() {
			ui.refresh(ac.app)
			modal := newArchiveModal(messages.String())
			pages.AddPage("modal", modal, true, false)
			pages.ShowPage("modal")
		})

		return err
	})

	if err := g.Wait(); err != nil {
		return context.Cause(ctx)
	}
	if messages.Len() > 0 {
		return errors.New(messages.String())
	}
	return nil
}

func newArchiveModal(message string) tview.Primitive {
	message += "\nYou can quit the program safely.\n\nPress the [enter] key to exit."
	lines := strings.Count(message, "\n")
	modal := func(p tview.Primitive, width, height int) tview.Primitive {
		return tview.NewFlex().
			AddItem(nil, 0, 1, false).
			AddItem(tview.NewFlex().SetDirection(tview.FlexRow).
				AddItem(nil, 0, 1, false).
				AddItem(p, height, 1, true).
				AddItem(nil, 0, 1, false), width, 1, true).
			AddItem(nil, 0, 1, false)
	}
	text := tview.NewTextView().SetText(message)
	box := tview.NewBox().SetBorder(true).SetTitle("Archive completed")
	text.Box = box
	return modal(text, 80, 2+lines)
}

func (ac *ArchiveCmd) newArchiveUI(a *app.Application) *archiveUI {
	ui := &archiveUI{
		counts:     make(map[fileevent.Code]*tview.TextView),
		extraViews: make(map[string]*tview.TextView),
	}
	if a.FileProcessor() != nil {
		ui.tracker = a.FileProcessor().Tracker()
		ui.fp = a.FileProcessor()
	}

	ui.screen = tview.NewGrid()

	header := tview.NewTextView().
		SetText(fmt.Sprintf("%s\nLocal index: %s files already archived", app.Banner(), formatCount(ac.indexCount))).
		SetDynamicColors(true)
	ui.screen.AddItem(header, 0, 0, 1, 1, 0, 0, false)

	ui.discoveryZone = ui.buildDiscoveryZone()
	ui.resultsZone = ui.buildResultsZone()

	zones := tview.NewGrid()
	zones.AddItem(ui.discoveryZone, 0, 0, 1, 1, 0, 0, false)
	zones.AddItem(ui.resultsZone, 0, 1, 1, 1, 0, 0, false)
	zones.SetColumns(35, 0)
	ui.screen.AddItem(zones, 1, 0, 1, 1, 0, 0, false)

	ui.logView = tview.NewTextView().SetMaxLines(100).ScrollToEnd()
	ui.logView.SetBorder(true).SetTitle("Log")
	ui.highJackLogger(a)
	ui.screen.AddItem(ui.logView, 2, 0, 1, 1, 0, 0, false)

	// banner ~5 rows, zones ~7, log fills the rest
	ui.screen.SetRows(5, 7, 0)

	return ui
}

func (ui *archiveUI) highJackLogger(a *app.Application) {
	ui.logView.SetDynamicColors(true)
	a.FileProcessor().Logger().SetLogger(a.Log().SetLogWriter(tview.ANSIWriter(ui.logView)))
}

func (ui *archiveUI) restoreLogger(a *app.Application) {
	a.FileProcessor().Logger().SetLogger(a.Log().SetLogWriter(nil))
}

func (ui *archiveUI) counter(code fileevent.Code) *tview.TextView {
	v, ok := ui.counts[code]
	if !ok {
		v = tview.NewTextView().SetText("     0").SetTextAlign(tview.AlignRight)
		ui.counts[code] = v
	}
	return v
}

func (ui *archiveUI) addRow(g *tview.Grid, row int, label string, code fileevent.Code) {
	g.AddItem(tview.NewTextView().SetText(label), row, 0, 1, 1, 0, 0, false)
	g.AddItem(ui.counter(code), row, 1, 1, 1, 0, 0, false)
}

func (ui *archiveUI) addExtraRow(g *tview.Grid, row int, label, key string) {
	v := tview.NewTextView().SetText("     0").SetTextAlign(tview.AlignRight)
	ui.extraViews[key] = v
	g.AddItem(tview.NewTextView().SetText(label), row, 0, 1, 1, 0, 0, false)
	g.AddItem(v, row, 1, 1, 1, 0, 0, false)
}

func (ui *archiveUI) buildDiscoveryZone() *tview.Grid {
	g := tview.NewGrid()
	g.SetBorder(true).SetTitle("Discovered")
	ui.addRow(g, 0, "Images", fileevent.DiscoveredImage)
	ui.addRow(g, 1, "Videos", fileevent.DiscoveredVideo)
	ui.addExtraRow(g, 2, "Total", "discoveredTotal")
	g.SetSize(3, 2, 1, 1).SetColumns(22, 0)
	return g
}

func (ui *archiveUI) buildResultsZone() *tview.Grid {
	g := tview.NewGrid()
	g.SetBorder(true).SetTitle("Results")
	ui.addRow(g, 0, "Downloaded (new)", fileevent.ProcessedFileArchived)
	ui.addRow(g, 1, "Skipped (no change)", fileevent.DiscardedLocalDuplicate)
	ui.addRow(g, 2, "Metadata updated", fileevent.ProcessedMetadataUpdated)
	ui.addRow(g, 3, "Moved / renamed", fileevent.ProcessedFileMoved)
	ui.addRow(g, 4, "Errors", fileevent.ErrorFileAccess)
	ui.addExtraRow(g, 5, "Processed", "processedTotal")
	g.SetSize(6, 2, 1, 1).SetColumns(22, 0)
	return g
}

func (ui *archiveUI) refresh(a *app.Application) {
	counts := a.FileProcessor().Logger().GetCounts()
	for code, view := range ui.counts {
		view.SetText(fmt.Sprintf("%6d", counts[code]))
	}

	discovered := counts[fileevent.DiscoveredImage] + counts[fileevent.DiscoveredVideo]
	if v, ok := ui.extraViews["discoveredTotal"]; ok {
		v.SetText(fmt.Sprintf("%6d", discovered))
	}

	processed := counts[fileevent.ProcessedFileArchived] + counts[fileevent.ProcessedMetadataUpdated] +
		counts[fileevent.ProcessedFileMoved] + counts[fileevent.DiscardedLocalDuplicate] +
		counts[fileevent.ErrorFileAccess]
	if v, ok := ui.extraViews["processedTotal"]; ok {
		if discovered > 0 {
			v.SetText(fmt.Sprintf("%d / %d", processed, discovered))
		} else {
			v.SetText(fmt.Sprintf("%d", processed))
		}
	}
}

func formatCount(n int) string {
	if n == 0 {
		return "0"
	}
	s := fmt.Sprintf("%d", n)
	result := make([]byte, 0, len(s)+(len(s)-1)/3)
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			result = append(result, ',')
		}
		result = append(result, byte(c))
	}
	return string(result)
}
