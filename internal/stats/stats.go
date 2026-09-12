// Package stats aggregates local usage from the session JSONL store (M15
// #72, the omp-stats equivalent): sessions, turns, tokens, reported cost,
// per-model and per-tool breakdowns, and a per-day rollup for `xdev stats`
// and the loopback dashboard.
//
// Two deliberate constraints:
//
//   - No SQLite sidecar (PRD non-goals: no `stats.db`-scale accumulation).
//     Repeat scans are cheap because of the tiny rollup cache in rollup.go.
//   - Scanning never materializes session entries through session.Open: a
//     single long session is tens of megabytes of tool output that stats
//     never reads, so lines are decoded into a lean wire struct instead
//     (peak memory stays at one line, well inside the RSS budget).
package stats

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"math"
	"os"
	"sort"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/session"
)

// Scan bounds. A user with years of history must not turn `xdev stats` into
// an unbounded read: the newest DefaultMaxFiles sessions are scanned, up to
// DefaultMaxBytes of JSONL. Both are reported in the output and widen with
// --limit / --max-bytes.
const (
	DefaultMaxFiles = 2000
	DefaultMaxBytes = 512 << 20
	// DefaultPort is the dashboard port (omp-stats parity).
	DefaultPort = 3847
	// DefaultRefresh is the dashboard's rescan interval.
	DefaultRefresh = 5 * time.Second
)

// Options selects what to scan.
type Options struct {
	// DataDir is the agent data dir (config.DataDir); empty = resolve it.
	DataDir string
	// Since drops sessions untouched before this time (zero = all time).
	Since time.Time
	// MaxFiles caps how many session files are scanned (0 = default).
	MaxFiles int
	// MaxBytes caps the total JSONL bytes read (0 = default).
	MaxBytes int64
	// NoRollup disables the on-disk cache (tests, cache debugging).
	NoRollup bool
}

// Totals is the whole-store aggregate.
type Totals struct {
	Sessions     int `json:"sessions"`
	Subagents    int `json:"subagentSessions"`
	UserMessages int `json:"userMessages"`
	Turns        int `json:"turns"`
	ToolCalls    int `json:"toolCalls"`
	ToolErrors   int `json:"toolErrors"`

	Input       int64 `json:"input"`
	Output      int64 `json:"output"`
	CacheRead   int64 `json:"cacheRead"`
	CacheWrite  int64 `json:"cacheWrite"`
	TotalTokens int64 `json:"totalTokens"`

	// CostUSD is what the providers actually reported (sum of
	// usage.cost.total). It is an estimate of spend, not a bill: a provider
	// that reports no cost contributes 0, which PricedTurns exposes.
	//
	// ponytail: no local price table, so a store behind a gateway that
	// reports nothing (local onegw) reads $0.00. Upgrade path: per-model
	// prices in models.yml, applied only when PricedTurns < Turns.
	CostUSD     float64 `json:"costUsd"`
	PricedTurns int     `json:"pricedTurns"`

	FirstSession time.Time `json:"firstSession,omitempty"`
	LastSession  time.Time `json:"lastSession,omitempty"`
}

// ModelStat is one model's share of the scan.
type ModelStat struct {
	Model       string  `json:"model"`
	Sessions    int     `json:"sessions"`
	Turns       int     `json:"turns"`
	Input       int64   `json:"input"`
	Output      int64   `json:"output"`
	CacheRead   int64   `json:"cacheRead"`
	TotalTokens int64   `json:"totalTokens"`
	CostUSD     float64 `json:"costUsd"`
}

// ToolStat is one tool's call count (Errors counts error results, which can
// exceed Calls on a wire that omits the call block).
type ToolStat struct {
	Name   string `json:"name"`
	Calls  int    `json:"calls"`
	Errors int    `json:"errors"`
}

// DayStat is one UTC day of activity.
type DayStat struct {
	Day      string  `json:"day"`
	Sessions int     `json:"sessions"`
	Turns    int     `json:"turns"`
	Tokens   int64   `json:"tokens"`
	CostUSD  float64 `json:"costUsd"`
}

// Distribution is the per-session spread (nearest-rank percentiles).
type Distribution struct {
	TurnsP50   int   `json:"turnsP50"`
	TurnsP90   int   `json:"turnsP90"`
	TurnsMax   int   `json:"turnsMax"`
	TokensP50  int64 `json:"tokensP50"`
	TokensP90  int64 `json:"tokensP90"`
	TokensMax  int64 `json:"tokensMax"`
	SessionsIn int   `json:"sessionsCounted"`
}

// Report is one scan.
type Report struct {
	GeneratedAt time.Time `json:"generatedAt"`
	DataDir     string    `json:"dataDir"`
	Since       string    `json:"since,omitempty"`

	FilesScanned    int   `json:"filesScanned"`
	FilesFiltered   int   `json:"filesFiltered"` // older than Since
	FilesOmitted    int   `json:"filesOmitted"`  // over the scan cap
	FilesUnreadable int   `json:"filesUnreadable"`
	CacheHits       int   `json:"cacheHits"`
	Truncated       bool  `json:"truncated"`
	MaxFiles        int   `json:"maxFiles"`
	MaxBytes        int64 `json:"maxBytes"`

	Totals       Totals       `json:"totals"`
	Models       []ModelStat  `json:"models"`
	Tools        []ToolStat   `json:"tools"`
	Days         []DayStat    `json:"days"`
	Distribution Distribution `json:"distribution"`

	// Fold accumulators. The index maps own the only pointers; finalize
	// freezes them into the exported slices and clears them. Never
	// serialized.
	modelIdx map[string]*ModelStat
	toolIdx  map[string]*ToolStat
	dayIdx   map[string]*DayStat
	sessions []sessionCounters
}

// Scan aggregates the session store under opts.DataDir.
func Scan(opts Options) (*Report, error) {
	if opts.DataDir == "" {
		opts.DataDir = config.DataDir()
	}
	if opts.MaxFiles <= 0 {
		opts.MaxFiles = DefaultMaxFiles
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = DefaultMaxBytes
	}
	metas, err := session.List(opts.DataDir)
	if err != nil {
		return nil, err
	}
	rep := &Report{
		GeneratedAt: time.Now().UTC(),
		DataDir:     opts.DataDir,
		MaxFiles:    opts.MaxFiles,
		MaxBytes:    opts.MaxBytes,
	}
	if !opts.Since.IsZero() {
		rep.Since = opts.Since.UTC().Format(time.RFC3339)
	}

	cache := rollup{}
	if !opts.NoRollup {
		cache = loadRollup(opts.DataDir)
	}
	fresh := rollup{}
	var bytes int64
	for _, m := range metas {
		if !opts.Since.IsZero() && m.ModTime.Before(opts.Since) {
			rep.FilesFiltered++
			continue
		}
		// Newest-first order, so the cap keeps the most recent history and
		// always reads at least one file (a single huge session still shows).
		if rep.FilesScanned >= opts.MaxFiles || (rep.FilesScanned > 0 && bytes >= opts.MaxBytes) {
			rep.FilesOmitted++
			rep.Truncated = true
			continue
		}
		c, ok := cache.get(m)
		if ok {
			rep.CacheHits++
		} else {
			c, err = readFileCounters(m.Path)
			if err != nil {
				rep.FilesUnreadable++
				continue
			}
		}
		fresh.put(m, c)
		bytes += m.SizeBytes
		rep.FilesScanned++
		rep.fold(m, c)
	}
	if !opts.NoRollup {
		// The rollup is an optimization, never data: a failed save costs a
		// slower next scan and is not worth failing the report over.
		_ = fresh.save(opts.DataDir)
	}
	rep.finalize()
	return rep, nil
}

// fold merges one file's counters into the report.
func (r *Report) fold(m session.SessionMeta, c counters) {
	t := &r.Totals
	t.Sessions++
	if m.TitleSource == "subagent" {
		t.Subagents++
	}
	t.UserMessages += c.UserMessages
	t.Turns += c.Turns
	t.ToolCalls += c.ToolCalls
	t.ToolErrors += c.ToolErrors
	t.Input += c.Input
	t.Output += c.Output
	t.CacheRead += c.CacheRead
	t.CacheWrite += c.CacheWrite
	t.TotalTokens += c.TotalTokens
	t.CostUSD += c.CostUSD
	t.PricedTurns += c.PricedTurns
	if !m.Timestamp.IsZero() {
		if t.FirstSession.IsZero() || m.Timestamp.Before(t.FirstSession) {
			t.FirstSession = m.Timestamp
		}
		if m.Timestamp.After(t.LastSession) {
			t.LastSession = m.Timestamp
		}
	}

	if len(c.Models) > 0 {
		byModel := r.modelIndex()
		for name, mc := range c.Models {
			st := byModel[name]
			if st == nil {
				st = &ModelStat{Model: name}
				byModel[name] = st
			}
			st.Sessions++
			st.Turns += mc.Turns
			st.Input += mc.Input
			st.Output += mc.Output
			st.CacheRead += mc.CacheRead
			st.TotalTokens += mc.TotalTokens
			st.CostUSD += mc.CostUSD
		}
	}
	if len(c.Tools) > 0 {
		byTool := r.toolIndex()
		for name, tc := range c.Tools {
			st := byTool[name]
			if st == nil {
				st = &ToolStat{Name: name}
				byTool[name] = st
			}
			st.Calls += tc.Calls
			st.Errors += tc.Errors
		}
	}
	if !m.Timestamp.IsZero() {
		r.dayByName(m.Timestamp.UTC().Format("2006-01-02")).Sessions++
	}
	for day, dc := range c.Days {
		d := r.dayByName(day)
		d.Turns += dc.Turns
		d.Tokens += dc.TotalTokens
		d.CostUSD += dc.CostUSD
	}

	r.sessions = append(r.sessions, sessionCounters{turns: c.Turns, tokens: c.TotalTokens})
}

// modelIndex / toolIndex / dayByName are the fold accumulators: map-keyed
// so growth never invalidates a pointer mid-fold. finalize is the only
// place that touches the exported slices.
func (r *Report) modelIndex() map[string]*ModelStat {
	if r.modelIdx == nil {
		r.modelIdx = map[string]*ModelStat{}
	}
	return r.modelIdx
}

func (r *Report) toolIndex() map[string]*ToolStat {
	if r.toolIdx == nil {
		r.toolIdx = map[string]*ToolStat{}
	}
	return r.toolIdx
}

func (r *Report) dayByName(day string) *DayStat {
	if r.dayIdx == nil {
		r.dayIdx = map[string]*DayStat{}
	}
	d := r.dayIdx[day]
	if d == nil {
		d = &DayStat{Day: day}
		r.dayIdx[day] = d
	}
	return d
}

// finalize freezes the accumulators into sorted exported slices and
// computes the distribution. It is the only place that writes the slices.
func (r *Report) finalize() {
	r.Models = make([]ModelStat, 0, len(r.modelIdx))
	for _, m := range r.modelIdx {
		r.Models = append(r.Models, *m)
	}
	sort.Slice(r.Models, func(i, j int) bool {
		if r.Models[i].TotalTokens != r.Models[j].TotalTokens {
			return r.Models[i].TotalTokens > r.Models[j].TotalTokens
		}
		return r.Models[i].Model < r.Models[j].Model
	})
	r.Tools = make([]ToolStat, 0, len(r.toolIdx))
	for _, t := range r.toolIdx {
		r.Tools = append(r.Tools, *t)
	}
	sort.Slice(r.Tools, func(i, j int) bool {
		if r.Tools[i].Calls != r.Tools[j].Calls {
			return r.Tools[i].Calls > r.Tools[j].Calls
		}
		return r.Tools[i].Name < r.Tools[j].Name
	})
	r.Days = make([]DayStat, 0, len(r.dayIdx))
	for _, d := range r.dayIdx {
		r.Days = append(r.Days, *d)
	}
	sort.Slice(r.Days, func(i, j int) bool { return r.Days[i].Day < r.Days[j].Day })

	turns := make([]int, 0, len(r.sessions))
	tokens := make([]int64, 0, len(r.sessions))
	for _, s := range r.sessions {
		turns = append(turns, s.turns)
		tokens = append(tokens, s.tokens)
	}
	sort.Ints(turns)
	sort.Slice(tokens, func(i, j int) bool { return tokens[i] < tokens[j] })
	r.Distribution = Distribution{
		TurnsP50:   percentileInt(turns, 0.50),
		TurnsP90:   percentileInt(turns, 0.90),
		TurnsMax:   maxInt(turns),
		TokensP50:  percentileInt64(tokens, 0.50),
		TokensP90:  percentileInt64(tokens, 0.90),
		TokensMax:  maxInt64(tokens),
		SessionsIn: len(turns),
	}
	r.modelIdx, r.toolIdx, r.dayIdx, r.sessions = nil, nil, nil, nil
}

type sessionCounters struct {
	turns  int
	tokens int64
}

// --- wire decode ---

// wireEntry is the lean projection of a session line. Field names follow the
// on-disk format; anything not named here (tool output, thinking blocks,
// image payloads) is scanned and dropped by the decoder without allocation.
type wireEntry struct {
	Type      string       `json:"type"`
	Timestamp time.Time    `json:"timestamp"`
	Message   *wireMessage `json:"message"`
}

type wireMessage struct {
	Role     string      `json:"role"`
	Model    string      `json:"model"`
	IsError  bool        `json:"isError"`
	ToolName string      `json:"toolName"`
	Usage    *ai.Usage   `json:"usage"`
	Content  []wireBlock `json:"content"`
}

type wireBlock struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

// counters is the per-file rollup: everything the report needs from one
// session file, small enough to cache thousands of.
type counters struct {
	UserMessages int     `json:"userMessages,omitempty"`
	Turns        int     `json:"turns,omitempty"`
	ToolCalls    int     `json:"toolCalls,omitempty"`
	ToolErrors   int     `json:"toolErrors,omitempty"`
	Input        int64   `json:"input,omitempty"`
	Output       int64   `json:"output,omitempty"`
	CacheRead    int64   `json:"cacheRead,omitempty"`
	CacheWrite   int64   `json:"cacheWrite,omitempty"`
	TotalTokens  int64   `json:"totalTokens,omitempty"`
	CostUSD      float64 `json:"costUsd,omitempty"`
	PricedTurns  int     `json:"pricedTurns,omitempty"`

	Models map[string]modelCounters `json:"models,omitempty"`
	Tools  map[string]toolCounters  `json:"tools,omitempty"`
	Days   map[string]dayCounters   `json:"days,omitempty"`
}

type modelCounters struct {
	Turns       int     `json:"turns,omitempty"`
	Input       int64   `json:"input,omitempty"`
	Output      int64   `json:"output,omitempty"`
	CacheRead   int64   `json:"cacheRead,omitempty"`
	TotalTokens int64   `json:"totalTokens,omitempty"`
	CostUSD     float64 `json:"costUsd,omitempty"`
}

type toolCounters struct {
	Calls  int `json:"calls,omitempty"`
	Errors int `json:"errors,omitempty"`
}

type dayCounters struct {
	Turns       int     `json:"turns,omitempty"`
	TotalTokens int64   `json:"totalTokens,omitempty"`
	CostUSD     float64 `json:"costUsd,omitempty"`
}

// readFileCounters streams one session file and accumulates its counters.
// A malformed line is skipped, not fatal: session files are append-only and
// a crash mid-append can leave a partial final line.
func readFileCounters(path string) (counters, error) {
	f, err := os.Open(path)
	if err != nil {
		return counters{}, err
	}
	defer f.Close()
	var c counters
	r := bufio.NewReaderSize(f, 64<<10)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			var e wireEntry
			if json.Unmarshal(bytes.TrimRight(line, "\r\n"), &e) == nil {
				c.addEntry(&e)
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return c, err
		}
	}
	return c, nil
}

func (c *counters) addEntry(e *wireEntry) {
	if e.Type != "message" || e.Message == nil {
		return
	}
	m := e.Message
	switch m.Role {
	case "user":
		c.UserMessages++
	case "assistant":
		c.Turns++
		name := m.Model
		if name == "" {
			name = "unknown"
		}
		if c.Models == nil {
			c.Models = map[string]modelCounters{}
		}
		mc := c.Models[name]
		mc.Turns++
		var turnTokens, turnCost float64
		if u := m.Usage; u != nil {
			c.Input += u.Input
			c.Output += u.Output
			c.CacheRead += u.CacheRead
			c.CacheWrite += u.CacheWrite
			total := u.TotalTokens
			if total == 0 {
				total = u.Input + u.Output + u.CacheRead + u.CacheWrite
			}
			c.TotalTokens += total
			mc.Input += u.Input
			mc.Output += u.Output
			mc.CacheRead += u.CacheRead
			mc.TotalTokens += total
			turnTokens = float64(total)
			if u.Cost != nil {
				c.CostUSD += u.Cost.Total
				mc.CostUSD += u.Cost.Total
				c.PricedTurns++
				turnCost = u.Cost.Total
			}
		}
		c.Models[name] = mc
		for _, b := range m.Content {
			if b.Type != "toolCall" {
				continue
			}
			c.ToolCalls++
			if c.Tools == nil {
				c.Tools = map[string]toolCounters{}
			}
			t := c.Tools[b.Name]
			t.Calls++
			c.Tools[b.Name] = t
		}
		if !e.Timestamp.IsZero() {
			if c.Days == nil {
				c.Days = map[string]dayCounters{}
			}
			day := e.Timestamp.UTC().Format("2006-01-02")
			d := c.Days[day]
			d.Turns++
			d.TotalTokens += int64(turnTokens)
			d.CostUSD += turnCost
			c.Days[day] = d
		}
	case "toolResult":
		if m.IsError {
			c.ToolErrors++
			if c.Tools == nil {
				c.Tools = map[string]toolCounters{}
			}
			name := m.ToolName
			if name == "" {
				name = "unknown"
			}
			t := c.Tools[name]
			t.Errors++
			c.Tools[name] = t
		}
	}
}

// percentileInt is the nearest-rank percentile (idx = ceil(p·n)−1): exact
// for the small n a session store has, no interpolation policy needed.
// Empty input is 0.
func percentileInt(sorted []int, p float64) int {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(p*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func percentileInt64(sorted []int64, p float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(p*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// maxInt / maxInt64 are the empty-tolerant maxima: slices.Max panics on an
// empty slice, and a session store with no turns is a normal case.
func maxInt(in []int) int {
	m := 0
	for _, v := range in {
		if v > m {
			m = v
		}
	}
	return m
}

func maxInt64(in []int64) int64 {
	var m int64
	for _, v := range in {
		if v > m {
			m = v
		}
	}
	return m
}
