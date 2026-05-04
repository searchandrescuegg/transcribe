// Command push-message is a synthetic trigger for the transcribe pipeline. It scans a
// directory of .wav files (default: docker/s3ninja/audio/), sorts them by the unix
// timestamp embedded in each filename, and publishes a real S3-event payload per file to
// the Pulsar input topic — one at a time with a configurable delay between sends. This is
// the same shape of payload Trunk-Recorder + the upstream S3 event hook produce in prod,
// so the local pipeline exercises the full Receive → Allow → S3-fetch → ASR → ML → Slack
// path.
//
// Filename convention (SDR-Trunk / Trunk-Recorder):
//
//	<talkgroup>-<unix_timestamp>_<frequency>.<suffix>.wav
//	e.g. 1399-1777832036_852162500.0-call_001.wav
//
// The timestamp is what we sort by; talkgroup is what determines the dispatch vs TAC
// branch in the pipeline. Files that don't match the format are skipped with a warning so
// you can drop fixtures and metadata files in the same directory without filtering them
// out by hand.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/apache/pulsar-client-go/pulsar"
	"github.com/versity/versitygw/s3event"
)

// filenameRE matches the leading <talkgroup>-<unix_timestamp> we need for routing and
// ordering. The rest of the filename can be anything trunk-recorder produced; we don't
// re-derive frequency or call-number here because the pipeline doesn't use them either.
var filenameRE = regexp.MustCompile(`^(\d+)-(\d+)_`)

type fixture struct {
	key       string // S3 object key — also the basename, since we mount the bucket flat
	talkgroup string
	timestamp int64
}

func main() {
	var (
		pulsarURL = flag.String("url", "pulsar://localhost:6650", "Pulsar service URL")
		topic     = flag.String("topic", "public/transcribe/file-queue", "Pulsar topic to publish events to")
		dir       = flag.String("dir", "docker/s3ninja/audio", "Directory of .wav fixtures (must already be mounted into s3-ninja's bucket)")
		bucket    = flag.String("bucket", "audio", "S3 bucket name to put in the event payload (must match S3_BUCKET in the service)")
		delay     = flag.Duration("delay", 3*time.Second, "Delay between successive event publishes")
		oneShot   = flag.String("file", "", "Optional: publish a single event for the named .wav (overrides -dir)")
	)
	flag.Parse()

	fixtures, err := collectFixtures(*dir, *oneShot)
	if err != nil {
		slog.Error("failed to collect fixtures", slog.String("error", err.Error()))
		os.Exit(1)
	}
	if len(fixtures) == 0 {
		slog.Error("no fixtures to publish; check -dir or -file", slog.String("dir", *dir))
		os.Exit(1)
	}

	client, err := pulsar.NewClient(pulsar.ClientOptions{URL: *pulsarURL})
	if err != nil {
		slog.Error("failed to create Pulsar client", slog.String("error", err.Error()), slog.String("url", *pulsarURL))
		os.Exit(1)
	}
	defer client.Close()

	producer, err := client.CreateProducer(pulsar.ProducerOptions{Topic: *topic})
	if err != nil {
		slog.Error("failed to create producer", slog.String("error", err.Error()), slog.String("topic", *topic))
		os.Exit(1)
	}
	defer producer.Close()

	ctx := context.Background()
	for i, f := range fixtures {
		if i > 0 {
			time.Sleep(*delay)
		}
		payload, err := json.Marshal(buildS3Event(*bucket, f.key))
		if err != nil {
			slog.Error("failed to marshal S3 event", slog.String("error", err.Error()), slog.String("key", f.key))
			continue
		}
		if _, err := producer.Send(ctx, &pulsar.ProducerMessage{Payload: payload}); err != nil {
			slog.Error("failed to publish event", slog.String("error", err.Error()), slog.String("key", f.key))
			continue
		}
		slog.Info("published S3 event",
			slog.String("key", f.key),
			slog.String("talkgroup", f.talkgroup),
			slog.Time("captured_at", time.Unix(f.timestamp, 0)),
			slog.Int("seq", i+1),
			slog.Int("total", len(fixtures)),
		)
	}
}

// collectFixtures returns the fixtures to publish, in chronological order. With -file set,
// returns just that one. With -dir, walks the directory and selects every .wav whose name
// matches the SDR-Trunk format.
func collectFixtures(dir, oneShot string) ([]fixture, error) {
	if oneShot != "" {
		f, ok := parseFilename(filepath.Base(oneShot))
		if !ok {
			return nil, fmt.Errorf("filename %q does not match expected format <tg>-<unix>_*.wav", oneShot)
		}
		return []fixture{f}, nil
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read dir %q: %w", dir, err)
	}
	var out []fixture
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".wav") {
			continue // skip $version, .json sidecars, .m4a, etc.
		}
		f, ok := parseFilename(name)
		if !ok {
			slog.Warn("skipping fixture: filename doesn't match expected format", slog.String("name", name))
			continue
		}
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].timestamp < out[j].timestamp })
	return out, nil
}

func parseFilename(name string) (fixture, bool) {
	m := filenameRE.FindStringSubmatch(name)
	if len(m) != 3 {
		return fixture{}, false
	}
	ts, err := strconv.ParseInt(m[2], 10, 64)
	if err != nil {
		return fixture{}, false
	}
	return fixture{key: name, talkgroup: m[1], timestamp: ts}, true
}

// buildS3Event produces the same shape the production S3 → Pulsar event hook emits. The
// pipeline only reads EventName + Records[].S3.Object.Key; the rest of the schema is
// padded with safe zero values so any future field-level access can't blow up.
func buildS3Event(bucket, key string) s3event.EventSchema {
	return s3event.EventSchema{
		Records: []s3event.EventRecord{{
			EventVersion: "2.1",
			EventSource:  "synthetic:push-message",
			AwsRegion:    "us-east-1",
			EventTime:    time.Now().UTC().Format(time.RFC3339),
			EventName:    s3event.EventObjectCreatedPut,
			S3: s3event.EventS3Data{
				Bucket: s3event.EventS3BucketData{Name: bucket},
				Object: s3event.EventObjectData{Key: key},
			},
		}},
	}
}
