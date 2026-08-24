// Command msgbackfill computes message-shape fingerprints for calls that were
// captured before those columns existed.
//
// WHY THIS IS A SEPARATE COMMAND AND NOT A STARTUP TASK. The fingerprint exists
// so the session view can avoid reading request bodies; computing it for old rows
// requires reading exactly those bodies once. On the production gateway that is
// ~26k captures averaging 1.78 MB, roughly 47 GB, out of a 70 GB file whose hot
// pages are scattered across the whole thing. Running that automatically on boot
// would mean a deploy quietly starting tens of GB of scattered IO against a box
// that also has to serve traffic — and sustained scanning of that database has
// already taken the host down hard enough to need a reboot.
//
// So it is opt-in, paced, and interruptible. The operator picks the window.
//
// Nothing depends on it: an un-backfilled row reads as "unknown", which the cover
// never treats as redundant, so the only cost of never running this is that old
// sessions keep reading every body the way they always did. New traffic is
// fingerprinted by the parse pipeline as it arrives, so the backlog shrinks on
// its own as the 7-day capture window rolls forward. This command only makes the
// improvement retroactive.
//
// Usage:
//
//	msgbackfill -db /path/to/songguo.db [-batch 100] [-pause 500ms] [-max 0] [-n]
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/songguo/songguo/internal/bodycodec"
	"github.com/songguo/songguo/internal/parse"
	"github.com/songguo/songguo/internal/store"
)

func main() {
	dbPath := flag.String("db", "", "path to the songguo SQLite database (required)")
	batch := flag.Int("batch", 100, "rows to read per batch")
	pause := flag.Duration("pause", 500*time.Millisecond, "idle time between batches, to leave IO for live traffic")
	max := flag.Int("max", 0, "stop after this many rows (0 = until done)")
	dry := flag.Bool("n", false, "report what would change without writing")
	flag.Parse()

	if *dbPath == "" {
		flag.Usage()
		os.Exit(2)
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		log.Fatalf("open %s: %v", *dbPath, err)
	}
	defer st.Close()

	// Ctrl-C stops between batches rather than mid-write. Partial progress is
	// fine: every fingerprint written is independently useful, and the rows left
	// behind are simply picked up by the next run.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var done, ok, unparseable int
	for {
		if ctx.Err() != nil {
			fmt.Println("interrupted; stopping between batches")
			break
		}
		if *max > 0 && done >= *max {
			break
		}

		limit := *batch
		if *max > 0 && *max-done < limit {
			limit = *max - done
		}

		rows, err := st.CallsNeedingFingerprint(limit)
		if err != nil {
			log.Fatalf("read batch: %v", err)
		}
		if len(rows) == 0 {
			break
		}

		for _, r := range rows {
			done++
			body := r.ReqBody
			enc := headerValue(r.ReqHeaders, "Content-Encoding")
			if decoded, attempted, derr := bodycodec.Decode(body, enc); attempted && derr == nil {
				body = decoded
			}

			c, _ := parse.Parse(parse.Input{Wire: r.Wire, ReqContentType: r.ReqContentType, ReqBody: body})
			f := c.Fingerprint()
			if f.Count == 0 {
				// No request messages recovered — a non-chat wire, or a body that
				// is compressed in a way we could not decode, or truncated. Mark it
				// so the next run does not read this body again; the cover still
				// treats it as unknown and reads it.
				unparseable++
				if !*dry {
					if err := st.MarkFingerprintUnavailable(r.CallID); err != nil {
						log.Printf("mark %s unavailable: %v", r.CallID, err)
					}
				}
				continue
			}
			ok++
			if !*dry {
				if err := st.SaveMessageFingerprint(r.CallID, f.Count, f.Head, f.Tail); err != nil {
					log.Printf("save %s: %v", r.CallID, err)
				}
			}
		}

		fmt.Printf("processed %d (fingerprinted %d, no messages %d)\n", done, ok, unparseable)

		// A short batch means there is nothing left to read.
		if len(rows) < limit {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(*pause):
		}
	}

	verb := "wrote"
	if *dry {
		verb = "would write"
	}
	fmt.Printf("done: read %d rows, %s %d fingerprints, %d had no request messages\n", done, verb, ok, unparseable)
}

// headerValue looks a header up case-insensitively, matching every other reader
// of captured headers in this codebase. Captured keys keep the client's casing,
// so an exact-match lookup would miss 'content-encoding' and leave a compressed
// body undecoded.
func headerValue(headers map[string]string, key string) string {
	for k, v := range headers {
		if strings.EqualFold(k, key) {
			return v
		}
	}
	return ""
}
