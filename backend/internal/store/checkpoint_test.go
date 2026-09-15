package store

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func openCheckpointStore(t *testing.T, cfg checkpointConfig) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ckpt.db")
	s, err := open(path, cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func fileSize(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// never keeps the loop from checkpointing on its own during a test.
var never = checkpointConfig{every: time.Hour, poll: time.Hour, walBytes: 1 << 62}

func savePayloadOf(t *testing.T, s *Store, size int) (string, []byte) {
	t.Helper()
	id := appendBareCall(t, s)
	body := bytes.Repeat([]byte{0xab, 0xcd, 0x01}, size/3)
	if err := s.SavePayload(Payload{CallID: id, ReqBody: body}); err != nil {
		t.Fatalf("SavePayload: %v", err)
	}
	return id, body
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A commit far past SQLite's 1000-page autocheckpoint threshold must leave its
// pages in the WAL: copying them into the database is the checkpointer's job,
// not the committing writer's.
func TestCommitsDoNotCheckpoint(t *testing.T) {
	s, path := openCheckpointStore(t, never)
	dbBefore := fileSize(path)

	savePayloadOf(t, s, 8<<20)

	if grew := fileSize(path) - dbBefore; grew > 1<<20 {
		t.Fatalf("database grew %d bytes during the commit; the writer checkpointed", grew)
	}
	if wal := fileSize(path + "-wal"); wal < 8<<20 {
		t.Fatalf("WAL is %d bytes, want the 8 MiB body still in it", wal)
	}

	s.checkpoint(context.Background())
	if grew := fileSize(path) - dbBefore; grew < 8<<20 {
		t.Fatalf("database grew %d bytes after a checkpoint, want the body copied in", grew)
	}
}

func TestCheckpointerRunsOnInterval(t *testing.T) {
	s, path := openCheckpointStore(t, checkpointConfig{every: 50 * time.Millisecond, poll: 10 * time.Millisecond, walBytes: 1 << 62})
	dbBefore := fileSize(path)
	savePayloadOf(t, s, 2<<20)
	waitFor(t, "an interval checkpoint to copy the body", func() bool { return fileSize(path)-dbBefore >= 2<<20 })
}

func TestCheckpointerRunsOnWALSize(t *testing.T) {
	s, path := openCheckpointStore(t, checkpointConfig{every: time.Hour, poll: 10 * time.Millisecond, walBytes: 1 << 20})
	dbBefore := fileSize(path)
	savePayloadOf(t, s, 4<<20)
	waitFor(t, "a size-triggered checkpoint to copy the body", func() bool { return fileSize(path)-dbBefore >= 4<<20 })
}

// Close stops the loop and SQLite's close checkpoints what remains, so nothing
// written is lost however long the interval.
func TestCloseKeepsUncheckpointedWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "close.db")
	s, err := open(path, never)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	id, body := savePayloadOf(t, s, 3<<20)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if wal := fileSize(path + "-wal"); wal != 0 {
		t.Errorf("WAL is %d bytes after Close, want it checkpointed away", wal)
	}

	s2, err := open(path, never)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	got, err := s2.GetPayload(id)
	if err != nil || !bytes.Equal(got.ReqBody, body) {
		t.Fatalf("payload after reopen: err %v, %d bytes (want %d identical)", err, len(got.ReqBody), len(body))
	}
}
