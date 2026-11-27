// Package tasks keeps a bounded, best-effort history of pull tasks on the hub.
//
// Providers bound their in-memory task list (the mock keeps the last 200) and
// lose it on restart, which makes the Pull Tasks view lose context right after
// a redeploy. The store here keeps a longer, paginated view of the tasks the hub
// has observed and optionally mirrors it to a JSONL file so the history survives
// a container restart.
//
// Persistence is deliberately simple: one JSON object per line
// ({"cluster":"...","task":{...}}), appended when a task is new or changes, and
// atomically compacted (tmp file + rename) once the file grows past
// compactFactor*capacity lines. The file is inspectable with plain tools and a
// line torn by a crash is skipped on load, so recovery is predictable. The store
// is stdlib only and never keeps the file handle open between writes.
package tasks

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/local/image-prepuller/internal/model"
)

const (
	// DefaultCapacity is the number of pull tasks kept when Open is called with
	// a non-positive capacity.
	DefaultCapacity = 2000

	// compactFactor bounds the append-only file: once it holds more than
	// compactFactor*capacity lines it is rewritten to exactly the records kept
	// in memory. With the default capacity that is a ~1 MiB file at most.
	compactFactor = 4

	// maxLineBytes bounds one JSONL line handled on load. A PullTask line is a
	// few hundred bytes; anything larger is a corrupt file we should not slurp.
	maxLineBytes = 1 << 20
)

// record is one line of the history file: the task plus the cluster it belongs
// to, because a task ID is only unique within a cluster.
type record struct {
	Cluster string         `json:"cluster"`
	Task    model.PullTask `json:"task"`
}

// Store is a concurrency-safe, bounded history of observed pull tasks. Records
// are kept newest first (by CreatedAt, then ID as a stable tie-break) so
// Query can paginate without sorting on every request.
type Store struct {
	mu       sync.Mutex
	path     string
	capacity int
	records  []record // newest first
	lines    int      // lines currently in the JSONL file
	closed   bool
}

// Open creates a task history. An empty path keeps it in memory only; a
// non-empty path loads the existing JSONL file (a missing file is fine), keeps
// the last version of every cluster+id pair, drops the oldest finished records
// above capacity and compacts the file atomically.
//
// The parent directory is never created implicitly: on a read-only root
// filesystem the caller is expected to mount a writable volume, so Open returns
// the underlying error and lets the caller fall back to a memory-only store.
func Open(path string, capacity int) (*Store, error) {
	if capacity <= 0 {
		capacity = DefaultCapacity
	}
	s := &Store{path: strings.TrimSpace(path), capacity: capacity}
	if s.path == "" {
		return s, nil
	}

	records, lines, err := load(s.path, capacity)
	if err != nil {
		return nil, err
	}
	s.records = records
	s.lines = lines

	// Probe writability up front: creating the file fails when the mounted
	// directory is missing, which is exactly the case main.go must be able to
	// fall back from instead of failing every Observe later.
	probe, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("pull task history %s: %w", s.path, err)
	}
	if err := probe.Close(); err != nil {
		return nil, fmt.Errorf("pull task history %s: %w", s.path, err)
	}

	// Compact immediately: the loaded file may hold superseded versions and
	// records above capacity.
	s.records = trimToCapacity(s.records, s.capacity)
	if err := writeAtomic(s.path, s.records); err != nil {
		return nil, fmt.Errorf("pull task history %s: %w", s.path, err)
	}
	s.lines = len(s.records)
	return s, nil
}

// Observe upserts the tasks a provider reported for one cluster. A state change
// (queued -> running -> succeeded/failed/canceled) overwrites the previous
// version; only new or changed records are appended to the history file. Write
// errors are logged and never surface to the HTTP request that triggered the
// observation.
func (s *Store) Observe(cluster string, batched []model.PullTask) {
	cluster = strings.TrimSpace(cluster)
	if cluster == "" || len(batched) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	changed := make([]record, 0, len(batched))
	for _, task := range batched {
		if task.ID == "" {
			continue
		}
		if task.Cluster == "" {
			task.Cluster = cluster
		}
		if s.upsertLocked(cluster, task) {
			changed = append(changed, record{Cluster: cluster, Task: task})
		}
	}
	if len(changed) == 0 {
		return
	}
	s.trimLocked()
	s.appendLocked(changed)
}

// Query returns one page of tasks of a cluster, newest first, plus the number of
// tasks matching the filter and the number of active (queued/running) tasks of
// that cluster. filter is "", "all" (no filtering), "active" (queued or
// running) or an exact TaskState. offset is clamped to zero, a non-positive
// limit returns no tasks.
func (s *Store) Query(cluster, filter string, offset, limit int) ([]model.PullTask, int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if offset < 0 {
		offset = 0
	}
	if limit < 0 {
		limit = 0
	}
	out := []model.PullTask{}
	total := 0
	active := 0
	pageEnd := offset + limit
	for _, rec := range s.records {
		if rec.Cluster != cluster {
			continue
		}
		if rec.Task.State.Active() {
			active++
		}
		if !matchFilter(rec.Task.State, filter) {
			continue
		}
		if total >= offset && total < pageEnd {
			out = append(out, rec.Task)
		}
		total++
	}
	return out, total, active
}

// Close flushes a compacted copy of the history and marks the store closed.
// It is safe to call it more than once.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.path == "" || s.lines == len(s.records) {
		return nil
	}
	if err := writeAtomic(s.path, s.records); err != nil {
		return fmt.Errorf("pull task history %s: %w", s.path, err)
	}
	s.lines = len(s.records)
	return nil
}

// upsertLocked stores one task and reports whether it is new or changed.
func (s *Store) upsertLocked(cluster string, task model.PullTask) bool {
	for i := range s.records {
		rec := s.records[i]
		if rec.Cluster != cluster || rec.Task.ID != task.ID {
			continue
		}
		if sameTask(rec.Task, task) {
			return false
		}
		// CreatedAt does not change between versions of one task, so the record
		// keeps its position in the newest-first slice.
		s.records[i].Task = task
		return true
	}
	pos := sort.Search(len(s.records), func(i int) bool {
		return !newer(s.records[i].Task, task)
	})
	s.records = append(s.records, record{})
	copy(s.records[pos+1:], s.records[pos:])
	s.records[pos] = record{Cluster: cluster, Task: task}
	return true
}

// trimLocked drops the oldest finished records above capacity. An active
// queued/running task is never dropped: a long running pull stays visible even
// when a burst of finished tasks would otherwise evict it.
func (s *Store) trimLocked() {
	s.records = trimToCapacity(s.records, s.capacity)
}

// trimToCapacity drops the oldest finished records above capacity, never an
// active queued/running one, and returns the trimmed slice.
func trimToCapacity(records []record, capacity int) []record {
	for len(records) > capacity {
		idx := -1
		for i := len(records) - 1; i >= 0; i-- {
			if !records[i].Task.State.Active() {
				idx = i
				break
			}
		}
		if idx < 0 {
			return records
		}
		records = append(records[:idx], records[idx+1:]...)
	}
	return records
}

// appendLocked writes the changed records to the history file and compacts it
// when the append-only file outgrows compactFactor*capacity lines.
func (s *Store) appendLocked(changed []record) {
	if s.path == "" {
		return
	}
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		log.Printf("pull task history: append %s: %v", s.path, err)
		return
	}
	w := bufio.NewWriter(f)
	written := 0
	for _, rec := range changed {
		line, err := json.Marshal(rec)
		if err != nil {
			log.Printf("pull task history: encode %s: %v", s.path, err)
			continue
		}
		if _, err := w.Write(append(line, '\n')); err != nil {
			log.Printf("pull task history: append %s: %v", s.path, err)
			break
		}
		written++
	}
	if err := w.Flush(); err != nil {
		log.Printf("pull task history: append %s: %v", s.path, err)
	}
	if err := f.Close(); err != nil {
		log.Printf("pull task history: append %s: %v", s.path, err)
	}
	s.lines += written
	if s.lines > compactFactor*s.capacity {
		if err := writeAtomic(s.path, s.records); err != nil {
			log.Printf("pull task history: compact %s: %v", s.path, err)
			return
		}
		s.lines = len(s.records)
	}
}

// load parses a history file. A missing file is not an error. Later lines win
// for the same cluster+id, so the last observed state of every task survives.
// Corrupt or half-written lines (a crash during append) are skipped.
func load(path string, capacity int) ([]record, int, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("pull task history %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	index := make(map[string]int)
	var records []record
	lines := 0
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), maxLineBytes)
	for scanner.Scan() {
		lines++
		raw := strings.TrimSpace(scanner.Text())
		if raw == "" {
			continue
		}
		var rec record
		if err := json.Unmarshal([]byte(raw), &rec); err != nil {
			continue
		}
		if rec.Cluster == "" || rec.Task.ID == "" {
			continue
		}
		key := rec.Cluster + "\x00" + rec.Task.ID
		if pos, ok := index[key]; ok {
			records[pos] = rec
			continue
		}
		index[key] = len(records)
		records = append(records, rec)
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		return nil, 0, fmt.Errorf("pull task history %s: %w", path, err)
	}
	sort.SliceStable(records, func(i, j int) bool {
		return newer(records[i].Task, records[j].Task)
	})
	return trimToCapacity(records, capacity), lines, nil
}

// newer reports whether a is ordered before b in the newest-first view.
func newer(a, b model.PullTask) bool {
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.After(b.CreatedAt)
	}
	return a.ID > b.ID
}

// sameTask reports whether two versions of a pull task carry the same data.
func sameTask(a, b model.PullTask) bool {
	return a.State == b.State &&
		a.Attempt == b.Attempt &&
		a.Error == b.Error &&
		a.Actor == b.Actor &&
		a.Image == b.Image &&
		a.Node == b.Node &&
		a.Trigger == b.Trigger &&
		a.DurationMs == b.DurationMs &&
		a.CreatedAt.Equal(b.CreatedAt) &&
		sameTimePtr(a.StartedAt, b.StartedAt) &&
		sameTimePtr(a.FinishedAt, b.FinishedAt)
}

func sameTimePtr(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

// matchFilter applies the Query filter to one task state.
func matchFilter(state model.TaskState, filter string) bool {
	switch filter {
	case "", "all":
		return true
	case "active":
		return state.Active()
	default:
		return string(state) == filter
	}
}

// writeAtomic writes the whole history to path via a temporary file in the same
// directory and a rename, so a crash either leaves the previous file or the new
// one, never a half-written history.
func writeAtomic(path string, records []record) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".pull-tasks-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	w := bufio.NewWriter(tmp)
	enc := json.NewEncoder(w)
	for _, rec := range records {
		if err := enc.Encode(rec); err != nil {
			_ = tmp.Close()
			return err
		}
	}
	if err := w.Flush(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
