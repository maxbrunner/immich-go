package archive

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/simulot/immich-go/app"
	"github.com/simulot/immich-go/internal/assettracker"
	"github.com/simulot/immich-go/internal/fileevent"
	"github.com/simulot/immich-go/internal/fileprocessor"
	"github.com/simulot/immich-go/internal/fshelper"
)

func newTestArchiveCmd(indexCount int) *ArchiveCmd {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := &app.Application{}
	a.SetLog(&app.Log{Logger: logger})
	a.SetFileProcessor(fileprocessor.New(assettracker.NewWithLogger(logger, false), fileevent.NewRecorder(logger)))
	return &ArchiveCmd{app: a, indexCount: indexCount}
}

// A scheduled archive run must not report success when the source gave it
// nothing. Before this check, a total API failure produced "Discovered: 0" and
// exit code 0, which reads as a healthy backup to a cron job.
func TestRunOutcome(t *testing.T) {
	ctx := context.Background()
	file := fshelper.FSName(nil, "IMG_1.jpg")

	tests := []struct {
		name    string
		setup   func(ac *ArchiveCmd)
		wantErr bool
	}{
		{
			name: "assets discovered, no error",
			setup: func(ac *ArchiveCmd) {
				ac.app.FileProcessor().RecordAssetDiscovered(ctx, file, 1, fileevent.DiscoveredImage)
			},
			wantErr: false,
		},
		{
			name: "source returned nothing against a populated archive",
			// no discovery recorded at all, indexCount > 0
			setup:   func(ac *ArchiveCmd) {},
			wantErr: true,
		},
		{
			name: "source error recorded through ProcessError",
			setup: func(ac *ArchiveCmd) {
				ac.app.FileProcessor().RecordAssetDiscovered(ctx, file, 1, fileevent.DiscoveredImage)
				_ = ac.app.ProcessError(errors.New("search/metadata failed"))
			},
			wantErr: true,
		},
		{
			name: "file write error",
			setup: func(ac *ArchiveCmd) {
				ac.app.FileProcessor().RecordAssetDiscovered(ctx, file, 1, fileevent.DiscoveredImage)
				ac.app.FileProcessor().RecordAssetError(ctx, file, 1, fileevent.ErrorFileAccess, errors.New("disk full"))
			},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ac := newTestArchiveCmd(100)
			tc.setup(ac)
			err := ac.runOutcome()
			if tc.wantErr && err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
		})
	}
}

// An empty destination with an empty source is a legitimate first run.
func TestRunOutcomeEmptyFirstRun(t *testing.T) {
	ac := newTestArchiveCmd(0)
	if err := ac.runOutcome(); err != nil {
		t.Fatalf("expected no error on an empty first run, got %v", err)
	}
}
