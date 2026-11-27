package agent

import (
	"fmt"
	"net/http"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// metricsExpositionVersion is the Prometheus text exposition format version
// written by the agent metrics handler.
const metricsExpositionVersion = "0.0.4"

// API endpoints as they appear in the endpoint label of the agent metrics.
// endpointOther aggregates every path that is not part of the agent API, so a
// wrong path shows up as traffic instead of silently disappearing.
const (
	endpointHealthz  = "healthz"
	endpointSnapshot = "snapshot"
	endpointImages   = "images"
	endpointPulls    = "pulls"
	endpointOther    = "other"
)

// lastContactEndpoints is the fixed set of last-contact series. All four exist
// from process start so "never contacted" is alertable; endpoints outside the
// set are counted but get no last-contact timestamp.
var lastContactEndpoints = []string{endpointHealthz, endpointSnapshot, endpointImages, endpointPulls}

// requestEndpoints is the fixed emission order of the request counters.
var requestEndpoints = []string{endpointHealthz, endpointSnapshot, endpointImages, endpointPulls, endpointOther}

// pullResults is the fixed set of prepuller_agent_pull_total result series.
// registry_auth is a registry rejection (missing or wrong credential), which is
// neither a dead registry nor an agent failure.
var pullResults = []string{"success", "cri_error", "stalled", "registry_unreachable", "registry_auth", "disk_paused"}

// requestKey identifies one prepuller_agent_requests_total counter.
type requestKey struct {
	endpoint string
	code     int
}

// Metrics records node-agent counters and gauges and renders them in the
// Prometheus text exposition format 0.0.4. It is safe for concurrent use and
// never blocks on the CRI or the network: the HTTP layer feeds it from data it
// has already collected, so a scrape costs one mutex and some formatting.
//
// The type deliberately has no package-level state: each Server owns exactly
// one Metrics, which keeps tests and embedded servers independent.
type Metrics struct {
	node      string
	version   string
	goVersion string
	startTime time.Time

	// now is swappable in tests to make timestamps deterministic.
	now func() time.Time

	mu              sync.Mutex
	lastContact     map[string]time.Time
	requests        map[requestKey]uint64
	criUp           bool
	pullInFlight    int64
	pullTotal       map[string]uint64
	imageFSUsed     int64
	imageFSCapacity int64
	imagesCached    int64
}

// NewMetrics builds the recorder for one node agent. The process start time is
// captured here and seeded into every last-contact series.
func NewMetrics(node, version string) *Metrics {
	start := time.Now()
	m := &Metrics{
		node:        node,
		version:     version,
		goVersion:   runtime.Version(),
		startTime:   start,
		now:         time.Now,
		lastContact: make(map[string]time.Time, len(lastContactEndpoints)),
		requests:    make(map[requestKey]uint64),
		pullTotal:   make(map[string]uint64, len(pullResults)),
	}
	for _, endpoint := range lastContactEndpoints {
		m.lastContact[endpoint] = start
	}
	for _, result := range pullResults {
		m.pullTotal[result] = 0
	}
	return m
}

// ObserveRequest counts one HTTP request and updates the last-contact
// timestamp of its endpoint. The status code is recorded as a label, so a 401
// from token rotation is distinguishable from a dead link.
func (m *Metrics) ObserveRequest(endpoint string, code int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests[requestKey{endpoint: endpoint, code: code}]++
	if _, ok := m.lastContact[endpoint]; ok {
		m.lastContact[endpoint] = m.now()
	}
}

// ObserveSnapshot records the outcome of one snapshot request. A failed
// snapshot flips cri_up to 0 but keeps the last known filesystem and cache
// gauges: stale last-known values are more useful for alerting than zeros that
// pretend the disk is empty. No extra CRI calls are made here; the handler
// feeds the snapshot it already built.
func (m *Metrics) ObserveSnapshot(ok bool, images, usedBytes, capacityBytes int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.criUp = ok
	if !ok {
		return
	}
	m.imagesCached = images
	m.imageFSUsed = usedBytes
	m.imageFSCapacity = capacityBytes
}

// PullStarted records that one CRI pull entered the in-flight window.
func (m *Metrics) PullStarted() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pullInFlight++
}

// PullFinished records the result of one pull attempt and leaves the in-flight
// window.
func (m *Metrics) PullFinished(result string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pullInFlight > 0 {
		m.pullInFlight--
	}
	m.pullTotal[result]++
}

// Handler serves the agent metrics on GET /metrics. Other methods answer 405:
// the endpoint is meant for Prometheus scrapes only and carries no auth.
func (m *Metrics) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version="+metricsExpositionVersion+"; charset=utf-8")
		_, _ = w.Write([]byte(m.render()))
	})
}

// render builds the whole exposition. Families, their series and the label
// order are emitted deterministically so scrapes of the same state are
// byte-identical.
func (m *Metrics) render() string {
	m.mu.Lock()
	node := m.node
	version := m.version
	goVersion := m.goVersion
	startTime := m.startTime
	lastContact := make(map[string]time.Time, len(m.lastContact))
	for endpoint, ts := range m.lastContact {
		lastContact[endpoint] = ts
	}
	requests := make(map[requestKey]uint64, len(m.requests))
	for key, count := range m.requests {
		requests[key] = count
	}
	criUp := m.criUp
	pullInFlight := m.pullInFlight
	pullTotal := make(map[string]uint64, len(m.pullTotal))
	for result, count := range m.pullTotal {
		pullTotal[result] = count
	}
	imageFSUsed := m.imageFSUsed
	imageFSCapacity := m.imageFSCapacity
	imagesCached := m.imagesCached
	m.mu.Unlock()

	var b strings.Builder
	header := func(name, help, typ string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
	}

	header("prepuller_agent_up",
		"Whether the node agent process is alive (always 1; the series disappears when the agent dies).",
		"gauge")
	metricLine(&b, "prepuller_agent_up", "1", "node", node)

	header("prepuller_agent_build_info",
		"Node agent build information (always 1; version labels carry the build).",
		"gauge")
	metricLine(&b, "prepuller_agent_build_info", "1", "node", node, "version", version, "go_version", goVersion)

	header("prepuller_agent_start_time_seconds",
		"Unix timestamp of the node agent process start.",
		"gauge")
	metricLine(&b, "prepuller_agent_start_time_seconds", strconv.FormatInt(startTime.Unix(), 10), "node", node)

	header("prepuller_agent_last_contact_timestamp_seconds",
		"Unix timestamp of the last request per endpoint (initialized at process start).",
		"gauge")
	for _, endpoint := range lastContactEndpoints {
		ts := lastContact[endpoint]
		metricLine(&b, "prepuller_agent_last_contact_timestamp_seconds", strconv.FormatInt(ts.Unix(), 10),
			"node", node, "endpoint", endpoint)
	}

	header("prepuller_agent_requests_total",
		"Total HTTP requests to the node agent API by endpoint and status code.",
		"counter")
	for _, endpoint := range requestEndpointOrder(requests) {
		codes := make([]int, 0, 4)
		for key := range requests {
			if key.endpoint == endpoint {
				codes = append(codes, key.code)
			}
		}
		sort.Ints(codes)
		for _, code := range codes {
			metricLine(&b, "prepuller_agent_requests_total",
				strconv.FormatUint(requests[requestKey{endpoint: endpoint, code: code}], 10),
				"node", node, "endpoint", endpoint, "code", strconv.Itoa(code))
		}
	}

	header("prepuller_agent_cri_up",
		"Whether the container runtime (CRI) answered the last snapshot request (1=up, 0=down).",
		"gauge")
	metricLine(&b, "prepuller_agent_cri_up", boolValue(criUp), "node", node)

	header("prepuller_agent_pull_in_flight",
		"Number of image pulls currently in flight.",
		"gauge")
	metricLine(&b, "prepuller_agent_pull_in_flight", strconv.FormatInt(pullInFlight, 10), "node", node)

	header("prepuller_agent_pull_total",
		"Total image pull attempts by result.",
		"counter")
	for _, result := range pullResultOrder(pullTotal) {
		metricLine(&b, "prepuller_agent_pull_total", strconv.FormatUint(pullTotal[result], 10),
			"node", node, "result", result)
	}

	header("prepuller_agent_imagefs_used_bytes",
		"Used bytes on the node image filesystem (last successful snapshot).",
		"gauge")
	metricLine(&b, "prepuller_agent_imagefs_used_bytes", strconv.FormatInt(imageFSUsed, 10), "node", node)

	header("prepuller_agent_imagefs_capacity_bytes",
		"Capacity bytes on the node image filesystem (last successful snapshot).",
		"gauge")
	metricLine(&b, "prepuller_agent_imagefs_capacity_bytes", strconv.FormatInt(imageFSCapacity, 10), "node", node)

	header("prepuller_agent_images_cached_total",
		"Number of images cached on the node (last successful snapshot).",
		"gauge")
	metricLine(&b, "prepuller_agent_images_cached_total", strconv.FormatInt(imagesCached, 10), "node", node)

	return b.String()
}

// requestEndpointOrder returns the fixed endpoint order followed by any
// unexpected endpoint labels in sorted order, so unknown values are not lost.
func requestEndpointOrder(requests map[requestKey]uint64) []string {
	order := append([]string(nil), requestEndpoints...)
	known := make(map[string]struct{}, len(requestEndpoints))
	for _, endpoint := range requestEndpoints {
		known[endpoint] = struct{}{}
	}
	var extra []string
	for key := range requests {
		if _, ok := known[key.endpoint]; ok {
			continue
		}
		known[key.endpoint] = struct{}{}
		extra = append(extra, key.endpoint)
	}
	sort.Strings(extra)
	return append(order, extra...)
}

// pullResultOrder returns the fixed result order followed by any unexpected
// result labels in sorted order.
func pullResultOrder(pullTotal map[string]uint64) []string {
	order := append([]string(nil), pullResults...)
	known := make(map[string]struct{}, len(pullResults))
	for _, result := range pullResults {
		known[result] = struct{}{}
	}
	var extra []string
	for result := range pullTotal {
		if _, ok := known[result]; ok {
			continue
		}
		known[result] = struct{}{}
		extra = append(extra, result)
	}
	sort.Strings(extra)
	return append(order, extra...)
}

// metricLine writes one sample line with pairs of label name and label value.
func metricLine(b *strings.Builder, name, value string, labels ...string) {
	b.WriteString(name)
	if len(labels) > 0 {
		b.WriteByte('{')
		for i := 0; i+1 < len(labels); i += 2 {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(labels[i])
			b.WriteString("=\"")
			b.WriteString(escapeLabelValue(labels[i+1]))
			b.WriteByte('"')
		}
		b.WriteByte('}')
	}
	b.WriteByte(' ')
	b.WriteString(value)
	b.WriteByte('\n')
}

// escapeLabelValue escapes a Prometheus label value: only backslash, double
// quote and newline have a defined escape in the text format.
func escapeLabelValue(value string) string {
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, "\"", "\\\"")
	value = strings.ReplaceAll(value, "\n", "\\n")
	return value
}

func boolValue(v bool) string {
	if v {
		return "1"
	}
	return "0"
}
