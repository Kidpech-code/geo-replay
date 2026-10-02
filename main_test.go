package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const sampleGPX = `<?xml version="1.0"?>
<gpx version="1.1" creator="geo-replay" xmlns="http://www.topografix.com/GPX/1/1">
  <wpt lat="0" lon="0"><time>2026-10-02T00:00:00Z</time></wpt>
  <trk><trkseg>
    <trkpt lat="13.7563" lon="100.5018"><time>2026-10-02T00:00:00Z</time></trkpt>
    <trkpt lat="13.7564" lon="100.5019"><time>2026-10-02T00:00:01Z</time></trkpt>
  </trkseg></trk>
</gpx>`

func writeGPX(t *testing.T, data string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "route.gpx")
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunSendsTrackPointsInOrder(t *testing.T) {
	got := make(chan pointPayload, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("unexpected request: %s %s", r.Method, r.Header.Get("Content-Type"))
		}
		var point pointPayload
		if err := json.NewDecoder(r.Body).Decode(&point); err != nil {
			t.Errorf("decode request: %v", err)
		}
		got <- point
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	before := time.Now().Add(-time.Second)
	err := run(context.Background(), []string{"-file", writeGPX(t, sampleGPX), "-speed", "1000", "-url", server.URL}, io.Discard)
	after := time.Now().Add(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("received %d points, want 2", len(got))
	}
	first, second := <-got, <-got
	if first.Latitude != 13.7563 || first.Longitude != 100.5018 || second.Latitude != 13.7564 || second.Longitude != 100.5019 {
		t.Fatalf("unexpected point order: %+v, %+v", first, second)
	}
	if first.Timestamp.Before(before) || first.Timestamp.After(after) || second.Timestamp.Before(first.Timestamp) {
		t.Fatalf("timestamps are not replay times in order: %s, %s", first.Timestamp, second.Timestamp)
	}
}

func TestRunValidatesWholeTrackBeforePosting(t *testing.T) {
	cases := []struct {
		name  string
		gpx   string
		speed string
	}{
		{"invalid longitude", strings.Replace(sampleGPX, `lon="100.5019"`, `lon="181"`, 1), "1"},
		{"missing time", strings.Replace(sampleGPX, `<time>2026-10-02T00:00:01Z</time>`, ``, 1), "1"},
		{"backward time", strings.Replace(sampleGPX, `2026-10-02T00:00:01Z`, `2026-09-30T00:00:01Z`, 1), "1"},
		{"wrong namespace", strings.Replace(sampleGPX, `http://www.topografix.com/GPX/1/1`, `https://example.com/not-gpx`, 1), "1"},
		{"schedule overflow", sampleGPX, "1e-20"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
			defer cancel()
			err := run(ctx, []string{"-file", writeGPX(t, tc.gpx), "-speed", tc.speed, "-url", server.URL}, io.Discard)
			if err == nil || calls.Load() != 0 {
				t.Fatalf("want preflight error and zero POSTs, got err=%v calls=%d", err, calls.Load())
			}
		})
	}
}

func TestRunStopsOnHTTPFailure(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	err := run(context.Background(), []string{"-file", writeGPX(t, sampleGPX), "-url", server.URL}, io.Discard)
	if err == nil || calls.Load() != 1 {
		t.Fatalf("want HTTP error after one POST, got err=%v calls=%d", err, calls.Load())
	}
}

func TestRunReusesHTTPConnectionWithResponseBody(t *testing.T) {
	var connections atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "2")
		if _, err := w.Write([]byte("ok")); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.Start()
	defer server.Close()

	if err := run(context.Background(), []string{"-file", writeGPX(t, sampleGPX), "-speed", "1000", "-url", server.URL}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if connections.Load() != 1 {
		t.Fatalf("opened %d TCP connections for two points, want 1", connections.Load())
	}
}

func TestRunPreviewsNDJSONWithoutURL(t *testing.T) {
	var output bytes.Buffer
	if err := run(context.Background(), []string{"-file", writeGPX(t, sampleGPX), "-speed", "1000"}, &output); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d JSON lines, want 2: %q", len(lines), output.String())
	}
	var point pointPayload
	if err := json.Unmarshal([]byte(lines[0]), &point); err != nil || point.Latitude != 13.7563 {
		t.Fatalf("unexpected first line: %q, err=%v", lines[0], err)
	}
}

func TestRunMyAPIPreviewsExactBatch(t *testing.T) {
	var output bytes.Buffer
	if err := run(context.Background(), []string{"-file", writeGPX(t, sampleGPX), "-mode", "my-api"}, &output); err != nil {
		t.Fatal(err)
	}
	want := `[{"lng":100.5018,"lat":13.7563},{"lng":100.5019,"lat":13.7564}]` + "\n"
	if output.String() != want {
		t.Fatalf("payload = %q, want %q", output.String(), want)
	}
}

func TestRunMyAPIPostsOneBatchAndReturnsResults(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/api/v1/geo/reverse/batch" || r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("unexpected request: %s %s %s", r.Method, r.URL.Path, r.Header.Get("Content-Type"))
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
		}
		want := `[{"lng":100.5018,"lat":13.7563},{"lng":100.5019,"lat":13.7564}]`
		if string(body) != want {
			t.Errorf("body = %s, want %s", body, want)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"success":true,"message":"ok","data":[{"lng":100.5018,"lat":13.7563,"matched":true},{"lng":100.5019,"lat":13.7564,"matched":false}]}`)
	}))
	defer server.Close()

	var output bytes.Buffer
	err := run(context.Background(), []string{"-file", writeGPX(t, sampleGPX), "-mode", "my-api", "-url", server.URL + "/api/v1/geo/reverse/batch"}, &output)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || !strings.Contains(output.String(), `"matched":false`) {
		t.Fatalf("calls=%d output=%q", calls.Load(), output.String())
	}
}

func TestRunMyAPIRejectsInvalidBatchBeforePosting(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	for _, tc := range []struct {
		name string
		gpx  string
	}{
		{"outside Thailand", strings.Replace(sampleGPX, `lat="13.7564"`, `lat="22"`, 1)},
		{"too many points", makeGPXPoints(1001)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := run(context.Background(), []string{"-file", writeGPX(t, tc.gpx), "-mode", "my-api", "-url", server.URL}, io.Discard)
			if err == nil || calls.Load() != 0 {
				t.Fatalf("want preflight error and zero POSTs, got err=%v calls=%d", err, calls.Load())
			}
		})
	}
}

func TestRunMyAPIRejectsSpeed(t *testing.T) {
	err := run(context.Background(), []string{"-file", writeGPX(t, sampleGPX), "-mode", "my-api", "-speed", "60"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "speed") {
		t.Fatalf("want incompatible speed error, got %v", err)
	}
}

func TestRunMyAPIRejectsIncompleteResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"success":true,"data":[{"matched":true}]}`)
	}))
	defer server.Close()

	var output bytes.Buffer
	err := run(context.Background(), []string{"-file", writeGPX(t, sampleGPX), "-mode", "my-api", "-url", server.URL}, &output)
	if err == nil || output.Len() != 0 {
		t.Fatalf("want error and no partial output, got err=%v output=%q", err, output.String())
	}
}

func TestRunMyAPIRejectsMalformedResults(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"success":true,"data":[null,{}]}`)
	}))
	defer server.Close()

	var output bytes.Buffer
	err := run(context.Background(), []string{"-file", writeGPX(t, sampleGPX), "-mode", "my-api", "-url", server.URL}, &output)
	if err == nil || output.Len() != 0 {
		t.Fatalf("want error and no partial output, got err=%v output=%q", err, output.String())
	}
}

func makeGPXPoints(n int) string {
	var gpx strings.Builder
	gpx.WriteString(`<gpx version="1.1" xmlns="http://www.topografix.com/GPX/1/1"><trk><trkseg>`)
	for range n {
		gpx.WriteString(`<trkpt lat="13.75" lon="100.50"><time>2026-10-02T00:00:00Z</time></trkpt>`)
	}
	gpx.WriteString(`</trkseg></trk></gpx>`)
	return gpx.String()
}
