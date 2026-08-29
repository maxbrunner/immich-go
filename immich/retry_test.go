package immich_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/simulot/immich-go/immich"
)

// A hosted Immich behind a gateway returns 502 whenever the upstream stalls.
// One of those used to fail the call outright, which cost a 2h15m archive run
// at 38% completion. These pin the retry behaviour that fixes it.

func newClient(t *testing.T, url string) *immich.ImmichClient {
	t.Helper()
	ic, err := immich.NewImmichClient(url, "key",
		immich.OptionRetriesDelay(5*time.Millisecond)) // keep the suite fast
	if err != nil {
		t.Fatalf("NewImmichClient: %v", err)
	}
	return ic
}

func TestRetriesTransientGetFailure(t *testing.T) {
	for _, status := range []int{429, 500, 502, 503, 504} {
		t.Run(fmt.Sprintf("%d then success", status), func(t *testing.T) {
			var calls int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if atomic.AddInt32(&calls, 1) == 1 {
					w.WriteHeader(status)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"id":"a1","originalFileName":"IMG_1.jpg","type":"IMAGE","checksum":"c1"}`)
			}))
			defer srv.Close()

			a, err := newClient(t, srv.URL).GetAssetInfo(context.Background(), "a1")
			if err != nil {
				t.Fatalf("expected the retry to succeed, got %v", err)
			}
			if a.ID != "a1" {
				t.Errorf("got asset %q, want a1", a.ID)
			}
			if got := atomic.LoadInt32(&calls); got != 2 {
				t.Errorf("server saw %d calls, want 2", got)
			}
		})
	}
}

func TestDoesNotRetryClientErrors(t *testing.T) {
	// 404 and 401 are answers, not blips. Retrying them wastes time and hides
	// a real problem behind a delay.
	for _, status := range []int{400, 401, 403, 404} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&calls, 1)
				w.WriteHeader(status)
			}))
			defer srv.Close()

			_, err := newClient(t, srv.URL).GetAssetInfo(context.Background(), "a1")
			if err == nil {
				t.Fatal("expected an error")
			}
			if got := atomic.LoadInt32(&calls); got != 1 {
				t.Errorf("server saw %d calls, want exactly 1", got)
			}
		})
	}
}

func TestGivesUpAndSaysHowManyTries(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	_, err := newClient(t, srv.URL).GetAssetInfo(context.Background(), "a1")
	if err == nil {
		t.Fatal("expected an error after exhausting retries")
	}
	if got := atomic.LoadInt32(&calls); got != 4 {
		t.Errorf("server saw %d calls, want 4 (1 attempt + 3 retries)", got)
	}
	// A run that only failed after several tries should say so, otherwise the
	// log looks identical to a single clean failure.
	if !strings.Contains(err.Error(), "after 4 attempts") {
		t.Errorf("error should report the attempt count, got: %v", err)
	}
}

func TestRetriesSearchPost(t *testing.T) {
	// /search/metadata is a POST, but it is a query: replaying it is safe, and
	// a 502 there aborts the whole enumeration rather than one asset.
	var calls int32
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		bodies = append(bodies, string(buf))
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"assets":{"total":1,"count":1,"nextPage":null,"items":[
			{"id":"a1","originalFileName":"IMG_1.jpg","type":"IMAGE","checksum":"c1"}]}}`)
	}))
	defer srv.Close()

	var seen []string
	err := newClient(t, srv.URL).GetAllAssetsWithFilter(context.Background(),
		&immich.SearchMetadataQuery{Page: 1}, func(a *immich.Asset) error {
			seen = append(seen, a.ID)
			return nil
		})
	if err != nil {
		t.Fatalf("expected the retried search to succeed, got %v", err)
	}
	if len(seen) != 1 {
		t.Fatalf("got %d assets, want 1", len(seen))
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("server saw %d calls, want 2", got)
	}
	// The replayed request must carry the same body, or the retry silently
	// searches for something else.
	if len(bodies) == 2 && bodies[0] != bodies[1] {
		t.Errorf("replayed body differs:\nfirst:  %q\nsecond: %q", bodies[0], bodies[1])
	}
}

func TestRetryStopsOnContextCancel(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(10 * time.Millisecond); cancel() }()

	ic, err := immich.NewImmichClient(srv.URL, "key",
		immich.OptionRetriesDelay(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, _ = ic.GetAssetInfo(ctx, "a1")
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("cancellation should abandon the backoff promptly, took %v", elapsed)
	}
}
