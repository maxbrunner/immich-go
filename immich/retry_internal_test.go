package immich

import (
	"bytes"
	"io"
	"net/http"
	"testing"
)

// Retrying is gated on the request being replayable. Uploads stream a multipart
// body that cannot be regenerated, so they must never qualify: a second attempt
// would send a truncated request. This pins that boundary, since it is the one
// place where a retry could do damage rather than just waste a second.
func TestOnlyReplayableRequestsAreRetried(t *testing.T) {
	tests := []struct {
		name  string
		build func() *http.Request
		want  bool
	}{
		{
			name: "GET has no body",
			build: func() *http.Request {
				r, _ := http.NewRequest(http.MethodGet, "http://x/api/assets/a1", nil)
				return r
			},
			want: true,
		},
		{
			name: "JSON body is regenerable",
			build: func() *http.Request {
				r, _ := http.NewRequest(http.MethodPost, "http://x/api/search/metadata", nil)
				sc := &serverCall{}
				if err := setJSONBody(map[string]int{"page": 1})(sc, r); err != nil {
					t.Fatal(err)
				}
				return r
			},
			want: true,
		},
		{
			name: "upload body is a one-shot stream",
			build: func() *http.Request {
				r, _ := http.NewRequest(http.MethodPost, "http://x/api/assets", nil)
				sc := &serverCall{}
				body := io.NopCloser(bytes.NewReader([]byte("multipart payload")))
				if err := setBody(body)(sc, r); err != nil {
					t.Fatal(err)
				}
				return r
			},
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := replayable(tc.build()); got != tc.want {
				t.Errorf("replayable = %v, want %v", got, tc.want)
			}
		})
	}
}

// A replayed JSON body must be byte-identical, or the retry quietly asks the
// server a different question than the first attempt did.
func TestReplayedJSONBodyIsIdentical(t *testing.T) {
	r, _ := http.NewRequest(http.MethodPost, "http://x/api/search/metadata", nil)
	sc := &serverCall{}
	if err := setJSONBody(map[string]any{"page": 2, "size": 1000})(sc, r); err != nil {
		t.Fatal(err)
	}
	first, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	again, err := replayBody(r)
	if err != nil {
		t.Fatal(err)
	}
	second, err := io.ReadAll(again)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Errorf("replayed body differs:\nfirst:  %q\nsecond: %q", first, second)
	}
	if r.ContentLength != int64(len(first)) {
		t.Errorf("ContentLength %d does not match body length %d", r.ContentLength, len(first))
	}
}
