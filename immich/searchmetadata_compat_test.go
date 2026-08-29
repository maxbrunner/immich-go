package immich_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/simulot/immich-go/immich"
)

// Immich v3 changed AssetResponseDto.duration from a string ("0:00:00.00000")
// to a number of milliseconds that may also be null. Because a single
// unmarshalling type error fails the whole /search/metadata page, decoding it
// into the wrong Go type made every search return zero assets while the run
// still looked healthy. These tests pin both response shapes.
func TestSearchMetadataDecodesBothServerGenerations(t *testing.T) {
	const v2Page = `{"assets":{"total":1,"count":1,"nextPage":null,"items":[
		{"id":"a1","originalFileName":"IMG_1.jpg","type":"IMAGE","checksum":"c1","duration":"0:00:00.00000"}]}}`

	// v3: numeric duration on the image, and null on nothing in particular —
	// both shapes appear in the wild depending on asset type.
	const v3Page = `{"assets":{"total":2,"count":2,"nextPage":null,"items":[
		{"id":"a1","originalFileName":"IMG_1.jpg","type":"IMAGE","checksum":"c1","duration":null},
		{"id":"a2","originalFileName":"VID_1.mp4","type":"VIDEO","checksum":"c2","duration":12345}]}}`

	tests := []struct {
		name    string
		page    string
		wantIDs []string
	}{
		{name: "immich v2", page: v2Page, wantIDs: []string{"a1"}},
		{name: "immich v3", page: v3Page, wantIDs: []string{"a1", "a2"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, tc.page)
			}))
			defer srv.Close()

			ic, err := immich.NewImmichClient(srv.URL, "key")
			if err != nil {
				t.Fatalf("NewImmichClient: %v", err)
			}

			var got []string
			err = ic.GetAllAssetsWithFilter(context.Background(), &immich.SearchMetadataQuery{Page: 1},
				func(a *immich.Asset) error {
					got = append(got, a.ID)
					return nil
				})
			if err != nil {
				t.Fatalf("GetAllAssetsWithFilter: %v", err)
			}
			if len(got) != len(tc.wantIDs) {
				t.Fatalf("got %d assets %v, want %d %v", len(got), got, len(tc.wantIDs), tc.wantIDs)
			}
			for i, want := range tc.wantIDs {
				if got[i] != want {
					t.Errorf("asset %d: got %q, want %q", i, got[i], want)
				}
			}
		})
	}
}
