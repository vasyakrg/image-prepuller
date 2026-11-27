package stats

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/local/image-prepuller/internal/model"
)

// ErrStatsUnavailable reports that the statistics database is not connected
// right now: the operator enabled the pipeline but the hub has no working pool
// (yet or anymore). The HTTP layer turns it into a friendly "database
// unavailable" state, never into a bare 500.
var ErrStatsUnavailable = errors.New("statistics database is not available")

// openStore opens a store from a DSN. It is a variable so tests can replace the
// database with a fake without touching the retry loop.
var openStore = OpenPostgres

// Service owns the optional statistics pipeline end to end: it connects with
// backoff, runs the collector loop and serves the read side to the hub UI.
//
// The zero value is not usable; build it with NewService. A Service with an
// empty DSN is still valid and answers Enabled()=false, which is what the UI
// renders as "the administrator disabled statistics collection".
type Service struct {
	dsn      string
	cfg      Config
	clusters []Cluster

	mu          sync.RWMutex
	querier     Querier
	connectedAt time.Time
}

// NewService builds the statistics service. An empty DSN means the pipeline is
// disabled by configuration; clusters may be empty (nothing to sample).
func NewService(dsn string, cfg Config, clusters []Cluster) *Service {
	return &Service{
		dsn:      strings.TrimSpace(dsn),
		cfg:      cfg.withDefaults(),
		clusters: append([]Cluster(nil), clusters...),
	}
}

// Enabled reports whether the operator configured a statistics database. It is
// false for a nil service as well, so callers do not need a nil check.
func (s *Service) Enabled() bool {
	return s != nil && s.dsn != ""
}

// Run connects to PostgreSQL (retrying with backoff), runs the collector loop
// until ctx is done and reconnects if the writer stops. It blocks; callers run it
// in a goroutine. A disabled service returns immediately.
//
// Run owns the store: the collector loop closes it when ctx is canceled.
func (s *Service) Run(ctx context.Context, logf func(string, ...any)) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if !s.Enabled() {
		logf("stats: disabled: no DSN")
		return
	}
	if len(s.clusters) == 0 {
		logf("statistics: no clusters registered, pipeline not started")
		return
	}
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		store, err := openStore(ctx, s.dsn)
		if err != nil {
			logf("statistics: PostgreSQL unavailable (%v), retrying in %s", err, backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < time.Minute {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		s.attach(store)
		logf("statistics: writing to PostgreSQL every %s, retention %s, clusters: %s",
			s.cfg.Interval, s.cfg.Retention, strings.Join(s.clusterIDs(), ","))
		// Run blocks until ctx is canceled and closes the store on the way out.
		Run(ctx, s.cfg, s.clusters, store, logf)
		s.detach()
		if ctx.Err() != nil {
			return
		}
		logf("statistics: writer stopped, reconnecting")
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
	}
}

// Info reports whether statistics are enabled and whether the database answers.
// It pings the pool, so callers pass a request-scoped context with a timeout.
func (s *Service) Info(ctx context.Context) model.StatsInfo {
	now := time.Now()
	info := model.StatsInfo{
		Enabled:          s.Enabled(),
		Clusters:         []string{},
		CheckedAt:        now,
		IntervalSeconds:  int64(s.cfg.Interval / time.Second),
		RetentionSeconds: int64(s.cfg.Retention / time.Second),
	}
	if !info.Enabled {
		return info
	}
	info.Clusters = s.clusterIDs()
	querier, connectedAt, err := s.reader()
	if err != nil {
		info.Error = "нет соединения с базой статистики"
		return info
	}
	info.ConnectedAt = &connectedAt
	if err := querier.Ping(ctx); err != nil {
		info.Error = "база статистики недоступна"
		return info
	}
	info.OK = true
	return info
}

// Overview returns the aggregate header of the statistics tab.
func (s *Service) Overview(ctx context.Context, window time.Duration) (model.StatsOverview, error) {
	querier, err := s.requireQuerier()
	if err != nil {
		return model.StatsOverview{}, err
	}
	return querier.Overview(ctx, window)
}

// Nodes returns the newest sample of every known node.
func (s *Service) Nodes(ctx context.Context) ([]model.StatsNodeSample, error) {
	querier, err := s.requireQuerier()
	if err != nil {
		return nil, err
	}
	return querier.Nodes(ctx)
}

// Images returns one page of image presence intervals.
func (s *Service) Images(ctx context.Context, q model.StatsImageQuery) (model.StatsImagePage, error) {
	querier, err := s.requireQuerier()
	if err != nil {
		return model.StatsImagePage{}, err
	}
	return querier.Images(ctx, q)
}

// GCEvents returns one page of image-collector events.
func (s *Service) GCEvents(ctx context.Context, q model.StatsEventQuery) (model.StatsGCPage, error) {
	querier, err := s.requireQuerier()
	if err != nil {
		return model.StatsGCPage{}, err
	}
	return querier.GCEvents(ctx, q)
}

// Repulls returns one page of re-pulls after a collector removal.
func (s *Service) Repulls(ctx context.Context, q model.StatsEventQuery) (model.StatsRepullPage, error) {
	querier, err := s.requireQuerier()
	if err != nil {
		return model.StatsRepullPage{}, err
	}
	return querier.Repulls(ctx, q)
}

// Tasks returns one page of the pull-task log.
func (s *Service) Tasks(ctx context.Context, q model.StatsTaskQuery) (model.StatsTaskPage, error) {
	querier, err := s.requireQuerier()
	if err != nil {
		return model.StatsTaskPage{}, err
	}
	return querier.Tasks(ctx, q)
}

// reader returns the current read side and when it was connected.
func (s *Service) reader() (Querier, time.Time, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.querier == nil {
		return nil, time.Time{}, ErrStatsUnavailable
	}
	return s.querier, s.connectedAt, nil
}

func (s *Service) requireQuerier() (Querier, error) {
	querier, _, err := s.reader()
	return querier, err
}

func (s *Service) attach(store Store) {
	querier, ok := store.(Querier)
	if !ok {
		// A custom opener that cannot answer queries keeps the writer running and
		// the UI honest: reads report "unavailable" instead of panicking.
		querier = nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.querier = querier
	s.connectedAt = time.Now()
}

func (s *Service) detach() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.querier = nil
	s.connectedAt = time.Time{}
}

// clusterIDs lists the sampled cluster IDs in registration order.
func (s *Service) clusterIDs() []string {
	out := make([]string, 0, len(s.clusters))
	for _, cluster := range s.clusters {
		if cluster.ID == "" {
			continue
		}
		out = append(out, cluster.ID)
	}
	return out
}
