package jobs

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func openTest(t *testing.T, capacity int) *Store {
	t.Helper()

	s, err := Open(filepath.Join(t.TempDir(), "jobs.db"), capacity, 1<<20, 100)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { s.Close() })

	return s
}

func createTest(t *testing.T, s *Store, key string) Job {
	t.Helper()

	j, _, err := s.Create(context.Background(), key, "hash", []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}

	return j
}

func TestStoreIdempotencyConcurrent(t *testing.T) {
	s := openTest(t, 32)

	var wg sync.WaitGroup

	ids := make(chan string, 20)

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			j, _, err := s.Create(context.Background(), "same-key", "hash", []byte(`{}`))
			if err != nil {
				t.Error(err)
				return
			}

			ids <- j.ID
		}()
	}

	wg.Wait()
	close(ids)

	first := ""
	for id := range ids {
		if first == "" {
			first = id
		}

		if id != first {
			t.Fatal("duplicate job")
		}
	}

	_, _, err := s.Create(context.Background(), "same-key", "different", []byte(`{}`))
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("expected conflict: %v", err)
	}
}

func TestStoreCapacityAndReplay(t *testing.T) {
	s := openTest(t, 1)
	j := createTest(t, s, "one")

	_, _, err := s.Create(context.Background(), "two", "h", []byte(`{}`))
	if !errors.Is(err, ErrCapacity) {
		t.Fatalf("capacity: %v", err)
	}

	replay, created, err := s.Create(context.Background(), "one", "hash", nil)
	if err != nil || created || replay.ID != j.ID {
		t.Fatalf("replay at capacity: %+v %v %v", replay, created, err)
	}
}

func TestStoreDurabilityAndRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")

	s, err := Open(path, 32, 1<<20, 100)
	if err != nil {
		t.Fatal(err)
	}

	j := createTest(t, s, "one")
	if err = s.Transition(context.Background(), j.ID, "queued", "submitting", "", ""); err != nil {
		t.Fatal(err)
	}

	s.Close()

	s, err = Open(path, 32, 1<<20, 100)
	if err != nil {
		t.Fatal(err)
	}

	defer s.Close()

	if err = s.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}

	got, err := s.Get(context.Background(), j.ID)
	if err != nil || got.State != "uncertain" {
		t.Fatalf("recovery: %+v %v", got, err)
	}
}

func TestStoreCompareAndSwapAndQueuedCancel(t *testing.T) {
	s := openTest(t, 2)

	j := createTest(t, s, "one")
	if err := s.Cancel(context.Background(), j.ID); err != nil {
		t.Fatal(err)
	}

	if err := s.Transition(context.Background(), j.ID, "queued", "submitting", "", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale worker changed cancelled job: %v", err)
	}

	k := createTest(t, s, "two")

	_ = s.Transition(context.Background(), k.ID, "queued", "submitting", "", "")
	if err := s.Cancel(context.Background(), k.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("unsafe active cancellation: %v", err)
	}
}

func TestStoreFinishAtomicAndQuota(t *testing.T) {
	s := openTest(t, 2)
	j := createTest(t, s, "one")
	_ = s.Transition(context.Background(), j.ID, "queued", "submitting", "", "")

	_ = s.Transition(context.Background(), j.ID, "submitting", "running", j.ID, "")
	if err := s.Finish(context.Background(), j.ID, []Image{{Data: []byte("image"), Type: "image/png"}}); err != nil {
		t.Fatal(err)
	}

	got, _ := s.Get(context.Background(), j.ID)
	if got.State != "succeeded" || got.Images != 1 {
		t.Fatalf("bad finish: %+v", got)
	}

	data, _, err := s.Image(context.Background(), j.ID, 0)
	if err != nil || string(data) != "image" {
		t.Fatalf("image: %s %v", data, err)
	}

	k := createTest(t, s, "two")
	_ = s.Transition(context.Background(), k.ID, "queued", "submitting", "", "")

	_ = s.Transition(context.Background(), k.ID, "submitting", "running", k.ID, "")
	if err = s.Finish(context.Background(), k.ID, []Image{{Data: make([]byte, (1<<20)+1)}}); !errors.Is(err, ErrCapacity) {
		t.Fatalf("quota: %v", err)
	}

	got, _ = s.Get(context.Background(), k.ID)
	if got.State != "running" || got.Images != 0 {
		t.Fatal("partial write")
	}
}

func TestStoreJobLimit(t *testing.T) {
	s := openTest(t, 32)
	s.maxJobs = 1
	createTest(t, s, "one")

	_, _, err := s.Create(context.Background(), "two", "h", nil)
	if !errors.Is(err, ErrCapacity) {
		t.Fatalf("limit: %v", err)
	}
}

func TestStoreMissing(t *testing.T) {
	s := openTest(t, 2)

	_, err := s.Get(context.Background(), "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatal(fmt.Sprint(err))
	}
}

func TestStoreRejectsSecondProcessOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")

	s, err := Open(path, 2, 1<<20, 100)
	if err != nil {
		t.Fatal(err)
	}

	defer s.Close()

	second, err := Open(path, 2, 1<<20, 100)
	if err == nil {
		second.Close()
		t.Fatal("second worker owner accepted")
	}
}

func TestRetentionPrunesOnlyOldTerminalJobs(t *testing.T) {
	s := openTest(t, 4)
	old := createTest(t, s, "old")
	active := createTest(t, s, "active")
	fresh := createTest(t, s, "fresh")
	_ = s.Cancel(context.Background(), old.ID)
	_ = s.Cancel(context.Background(), fresh.ID)

	_, _ = s.db.Exec(`UPDATE jobs SET updated=? WHERE id IN (?,?)`, int64(1), old.ID, active.ID)
	if err := s.Prune(context.Background(), time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Get(context.Background(), old.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("terminal row retained: %v", err)
	}

	for _, id := range []string{active.ID, fresh.ID} {
		if _, err := s.Get(context.Background(), id); err != nil {
			t.Fatal("active/fresh row deleted", err)
		}
	}
}
