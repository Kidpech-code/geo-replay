package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	gpxNamespace = "http://www.topografix.com/GPX/1/1"
	maxGPXBytes  = 32 << 20
)

type pointPayload struct {
	Latitude  float64   `json:"latitude"`
	Longitude float64   `json:"longitude"`
	Timestamp time.Time `json:"timestamp"`
}

type trackPoint struct {
	Latitude  float64
	Longitude float64
	Time      time.Time
}

type gpxDocument struct {
	XMLName xml.Name   `xml:"http://www.topografix.com/GPX/1/1 gpx"`
	Version string     `xml:"version,attr"`
	Tracks  []gpxTrack `xml:"http://www.topografix.com/GPX/1/1 trk"`
}

type gpxTrack struct {
	Segments []gpxSegment `xml:"http://www.topografix.com/GPX/1/1 trkseg"`
}

type gpxSegment struct {
	Points []gpxTrackPoint `xml:"http://www.topografix.com/GPX/1/1 trkpt"`
}

type gpxTrackPoint struct {
	Latitude  string `xml:"lat,attr"`
	Longitude string `xml:"lon,attr"`
	Time      string `xml:"http://www.topografix.com/GPX/1/1 time"`
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "geo-replay:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("geo-replay", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	file := flags.String("file", "", "GPX 1.1 track file (required)")
	endpoint := flags.String("url", "", "HTTP endpoint for JSON POSTs; omit to preview NDJSON")
	speed := flags.Float64("speed", 1, "replay speed multiplier (must be positive)")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return fmt.Errorf("parse flags: %w", err)
	}
	if *file == "" || flags.NArg() != 0 {
		return errors.New("use -file route.gpx with no positional arguments")
	}
	if *speed <= 0 || math.IsNaN(*speed) || math.IsInf(*speed, 0) {
		return errors.New("speed must be a positive finite number")
	}
	if err := validateURL(*endpoint); err != nil {
		return err
	}
	points, err := readGPX(*file)
	if err != nil {
		return err
	}

	emit := func(_ context.Context, point pointPayload) error {
		return json.NewEncoder(out).Encode(point)
	}
	if *endpoint != "" {
		client := &http.Client{
			Timeout: 10 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
		emit = func(ctx context.Context, point pointPayload) error {
			return postPoint(ctx, client, *endpoint, point)
		}
	}
	return replay(ctx, points, *speed, emit)
}

func validateURL(raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return errors.New("url must be an absolute http:// or https:// URL")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("url must not contain credentials, query, or fragment")
	}
	return nil
}

func readGPX(path string) ([]trackPoint, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open GPX: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxGPXBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read GPX: %w", err)
	}
	if len(data) > maxGPXBytes {
		return nil, fmt.Errorf("GPX exceeds %d MiB", maxGPXBytes>>20)
	}
	return parseGPX(data)
}

func parseGPX(data []byte) ([]trackPoint, error) {
	var doc gpxDocument
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse GPX: %w", err)
	}
	if doc.XMLName.Space != gpxNamespace || doc.Version != "1.1" {
		return nil, errors.New("expected GPX 1.1 with the official namespace")
	}
	var points []trackPoint
	for _, track := range doc.Tracks {
		for _, segment := range track.Segments {
			for _, raw := range segment.Points {
				lat, err := strconv.ParseFloat(raw.Latitude, 64)
				if err != nil || math.IsNaN(lat) || lat < -90 || lat > 90 {
					return nil, fmt.Errorf("track point %d: invalid latitude", len(points)+1)
				}
				lon, err := strconv.ParseFloat(raw.Longitude, 64)
				if err != nil || math.IsNaN(lon) || lon < -180 || lon >= 180 {
					return nil, fmt.Errorf("track point %d: invalid longitude", len(points)+1)
				}
				at, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(raw.Time))
				if err != nil {
					return nil, fmt.Errorf("track point %d: time must include a timezone", len(points)+1)
				}
				if len(points) > 0 && at.Before(points[len(points)-1].Time) {
					return nil, fmt.Errorf("track point %d: time goes backwards", len(points)+1)
				}
				points = append(points, trackPoint{lat, lon, at})
			}
		}
	}
	if len(points) == 0 {
		return nil, errors.New("GPX has no track points")
	}
	return points, nil
}

func replay(ctx context.Context, points []trackPoint, speed float64, emit func(context.Context, pointPayload) error) error {
	offsets := make([]time.Duration, len(points))
	for i, point := range points {
		elapsed := point.Time.Sub(points[0].Time)
		if point.Time.After(points[0].Time.Add(elapsed)) {
			return fmt.Errorf("point %d: track interval is too large", i+1)
		}
		scaled := float64(elapsed) / speed
		if scaled >= float64(1<<63) {
			return fmt.Errorf("point %d: replay interval is too large for speed", i+1)
		}
		offsets[i] = time.Duration(scaled)
	}
	started := time.Now()
	for i, point := range points {
		// Anchor every point to the first timestamp so HTTP latency does not accumulate.
		wait := time.Until(started.Add(offsets[i]))
		if wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		payload := pointPayload{point.Latitude, point.Longitude, time.Now().UTC()}
		if err := emit(ctx, payload); err != nil {
			return fmt.Errorf("point %d: %w", i+1, err)
		}
	}
	return nil
}

func postPoint(ctx context.Context, client *http.Client, endpoint string, point pointPayload) error {
	body, err := json.Marshal(point)
	if err != nil {
		return fmt.Errorf("encode point: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("send request: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if err := resp.Body.Close(); err != nil {
			return fmt.Errorf("HTTP %d; close response: %w", resp.StatusCode, err)
		}
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	// ponytail: drain up to 64 KiB; raise the limit if large success bodies need connection reuse.
	_, readErr := io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	closeErr := resp.Body.Close()
	if readErr != nil {
		return fmt.Errorf("read response: %w", readErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close response: %w", closeErr)
	}
	return nil
}
