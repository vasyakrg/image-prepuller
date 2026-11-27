package tasks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/local/image-prepuller/internal/model"
)

func task(id string, state model.TaskState, created time.Time) model.PullTask {
	t := model.PullTask{
		ID:        id,
		Image:     "ghcr.io/acme/app:" + id,
		Node:      "talos-worker-01",
		State:     state,
		Trigger:   model.TriggerManual,
		CreatedAt: created,
	}
	if !state.Active() {
		finished := created.Add(time.Minute)
		t.FinishedAt = &finished
		t.DurationMs = 60000
	}
	return t
}

func TestStoreMemoryUpsertAndTrim(t *testing.T) {
	store, err := Open("", 2)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = store.Close() }()

	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	active := task("active", model.TaskQueued, base)
	store.Observe("kz", []model.PullTask{active})
	for i := 0; i < 3; i++ {
		store.Observe("kz", []model.PullTask{task("done-"+string(rune('a'+i)), model.TaskSucceeded, base.Add(time.Duration(i+1)*time.Minute))})
	}

	got, total, activeCount := store.Query("kz", "", 0, 10)
	if total != 2 || len(got) != 2 {
		t.Fatalf("total=%d len=%d, want capacity 2: %+v", total, len(got), got)
	}
	if activeCount != 1 {
		t.Fatalf("active = %d, want 1", activeCount)
	}
	// The active task was the oldest record: it must survive the trim.
	found := false
	for _, item := range got {
		if item.ID == "active" {
			found = true
		}
	}
	if !found {
		t.Fatalf("active queued task was trimmed: %+v", got)
	}

	// A state transition overwrites the record instead of adding a new one.
	store.Observe("kz", []model.PullTask{task("active", model.TaskRunning, base)})
	all, total, _ := store.Query("kz", "", 0, 10)
	if total != 2 {
		t.Fatalf("total after state transition = %d, want 2: %+v", total, all)
	}
	running, _, _ := store.Query("kz", "running", 0, 10)
	if len(running) != 1 || running[0].State != model.TaskRunning {
		t.Fatalf("running filter = %+v", running)
	}
}

func TestStoreFilterAndPagination(t *testing.T) {
	store, err := Open("", 100)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = store.Close() }()

	base := time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC)
	states := []model.TaskState{
		model.TaskQueued, model.TaskSucceeded, model.TaskFailed,
		model.TaskRunning, model.TaskCanceled,
	}
	for i, state := range states {
		store.Observe("kz", []model.PullTask{task("t-"+string(rune('0'+i)), state, base.Add(time.Duration(i)*time.Minute))})
	}
	// Same task IDs in another cluster must not leak into the first one.
	store.Observe("eu", []model.PullTask{task("t-0", model.TaskSucceeded, base)})

	page, total, active := store.Query("kz", "", 0, 2)
	if total != 5 || active != 2 {
		t.Fatalf("total=%d active=%d, want 5 2", total, active)
	}
	if len(page) != 2 || page[0].ID != "t-4" || page[1].ID != "t-3" {
		t.Fatalf("first page = %+v, want newest first t-4,t-3", page)
	}
	page, total, _ = store.Query("kz", "", 3, 10)
	if total != 5 || len(page) != 2 || page[0].ID != "t-1" || page[1].ID != "t-0" {
		t.Fatalf("second page = %+v (total %d), want t-1,t-0", page, total)
	}
	if page, _, _ := store.Query("kz", "", 10, 10); len(page) != 0 || page == nil {
		t.Fatalf("past-the-end page = %#v, want empty slice", page)
	}

	activePage, total, active := store.Query("kz", "active", 0, 10)
	if total != 2 || active != 2 || len(activePage) != 2 {
		t.Fatalf("active filter = %d/%d/%d, want 2/2/2", total, active, len(activePage))
	}
	failed, total, _ := store.Query("kz", "failed", 0, 10)
	if total != 1 || len(failed) != 1 || failed[0].ID != "t-2" {
		t.Fatalf("failed filter = %+v (total %d)", failed, total)
	}
	// Unknown filters are not the store's problem: they match nothing, and the
	// HTTP layer rejects them with 400 before they ever get here.
	if none, total, _ := store.Query("kz", "bogus", 0, 10); total != 0 || len(none) != 0 {
		t.Fatalf("bogus filter = %+v (total %d)", none, total)
	}
	// A non-positive limit returns no tasks but still reports the totals.
	if none, total, _ := store.Query("kz", "", 0, 0); len(none) != 0 || total != 5 {
		t.Fatalf("limit 0 = %+v (total %d)", none, total)
	}
	if none, _, _ := store.Query("eu", "", -3, 10); len(none) != 1 {
		t.Fatalf("negative offset query = %+v, want one eu task", none)
	}
}

func TestStorePersistsAndReloadsLastState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pull-tasks.jsonl")
	base := time.Date(2026, 3, 1, 8, 0, 0, 0, time.UTC)

	store, err := Open(path, 10)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	store.Observe("kz", []model.PullTask{task("a", model.TaskRunning, base)})
	store.Observe("kz", []model.PullTask{task("a", model.TaskSucceeded, base)})
	store.Observe("eu", []model.PullTask{task("b", model.TaskFailed, base.Add(time.Minute))})
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reloaded, err := Open(path, 10)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = reloaded.Close() }()

	got, total, active := reloaded.Query("kz", "", 0, 10)
	if total != 1 || len(got) != 1 || got[0].State != model.TaskSucceeded {
		t.Fatalf("reloaded kz = %+v (total %d, active %d), want the last succeeded state", got, total, active)
	}
	if got, total, _ := reloaded.Query("eu", "", 0, 10); total != 1 || got[0].ID != "b" {
		t.Fatalf("reloaded eu = %+v (total %d)", got, total)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("history file: %v", err)
	}
}

func TestStoreLoadSkipsCorruptLinesAndTrimsCapacity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pull-tasks.jsonl")
	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	valid := func(id string, state model.TaskState, created time.Time) string {
		rec := record{Cluster: "kz", Task: task(id, state, created)}
		line, err := json.Marshal(rec)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return string(line)
	}
	content := valid("old", model.TaskSucceeded, base) + "\n" +
		"{this is not json}\n" +
		"\n" +
		valid("new", model.TaskFailed, base.Add(2*time.Minute)) + "\n" +
		valid("running", model.TaskRunning, base.Add(3*time.Minute)) + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	store, err := Open(path, 2)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = store.Close() }()

	got, total, active := store.Query("kz", "", 0, 10)
	if total != 2 {
		t.Fatalf("total = %d, want 2 after capacity trim: %+v", total, got)
	}
	// Capacity 2 with two finished tasks older than the active one: the active
	// running task must survive and only the newest finished task is kept.
	if active != 1 {
		t.Fatalf("active = %d, want 1", active)
	}
	if got[0].ID != "running" || got[1].ID != "new" {
		t.Fatalf("trimmed records = %+v, want running,new", got)
	}
}

func TestStoreCompactsAppendOnlyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pull-tasks.jsonl")
	base := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)

	store, err := Open(path, 4)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = store.Close() }()

	// Flip one task between two states often enough to cross
	// compactFactor*capacity appended lines.
	for i := 0; i < compactFactor*4+5; i++ {
		state := model.TaskRunning
		if i%2 == 0 {
			state = model.TaskSucceeded
		}
		store.Observe("kz", []model.PullTask{task("flip", state, base)})
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read history: %v", err)
	}
	lines := 0
	for _, b := range data {
		if b == '\n' {
			lines++
		}
	}
	if lines > 4 {
		t.Fatalf("history file has %d lines, want it compacted to <= capacity (4): %s", lines, data)
	}
}

func TestStoreMemoryOnlyDoesNotTouchDisk(t *testing.T) {
	store, err := Open("", 10)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	store.Observe("kz", []model.PullTask{task("a", model.TaskSucceeded, time.Now())})
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Closing twice is fine and observes after Close are ignored.
	if err := store.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	store.Observe("kz", []model.PullTask{task("b", model.TaskSucceeded, time.Now())})
	if _, total, _ := store.Query("kz", "", 0, 10); total != 1 {
		t.Fatalf("observe after close changed the store: total %d", total)
	}
}

func TestStoreConcurrentObserveAndQuery(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "pull-tasks.jsonl"), 50)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = store.Close() }()

	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				id := "task-" + string(rune('a'+worker)) + "-" + string(rune('0'+i%10))
				store.Observe("kz", []model.PullTask{task(id, model.TaskRunning, base.Add(time.Duration(i)*time.Second))})
				_, _, _ = store.Query("kz", "", 0, 10)
			}
		}(w)
	}
	wg.Wait()
	if _, total, _ := store.Query("kz", "", 0, 100); total == 0 {
		t.Fatal("concurrent observe left no records")
	}
}

func TestStoreOpenFailsWhenDirectoryIsMissing(t *testing.T) {
	// The hub may run with a read-only root filesystem: a missing mounted
	// directory must surface as an error so main.go can fall back to memory
	// instead of silently creating directories outside the mount.
	path := filepath.Join(t.TempDir(), "no-such-dir", "pull-tasks.jsonl")
	if _, err := Open(path, 10); err == nil {
		t.Fatalf("Open(%q) = nil error, want the missing directory error", path)
	}
}
