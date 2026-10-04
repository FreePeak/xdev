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
	// Pricer looks up a local price for a model id (see
	// config.Config.Pricing) and returns nil for a model with none. It is
	// consulted ONLY for a request the provider priced in tokens alone.
	// Nil = every unpriced request counts as $0, exactly what a scan with
	// no models.yml has always done.
	Pricer func(model string) Pricer
}

// Pricer is one model's local per-million-token rates. A lookup returning nil
// is "no local price for this model": the request keeps the honest zero a
// silent gateway produces, and PricedRequests does not count it.
type Pricer interface {
	USD(input, output, cacheRead, cacheWrite, total int64) float64
}

// Totals is the whole-store aggregate.
type Totals struct {
	Sessions     int `json:"sessions"`
	Subagents    int `json:"subagentSessions"`
	UserMessages int `json:"userMessages"`
	// InjectedTurns counts harness-written user messages — provider
	// cut-off continuations, goal nudges — which are not user input
	// (#283: they inflated the felt turn count while hiding where the
	// turns actually came from).
	InjectedTurns int `json:"injectedTurns"`
	Turns         int `json:"turns"`
	ToolCalls     int `json:"toolCalls"`
	ToolErrors    int `json:"toolErrors"`

	Input       int64 `json:"input"`
	Output      int64 `json:"output"`
	CacheRead   int64 `json:"cacheRead"`
	CacheWrite  int64 `json:"cacheWrite"`
	TotalTokens int64 `json:"totalTokens"`

	// CostUSD is what the session cost: every provider-reported
	// usage.cost.total, plus a local estimate (CostEstimated) for the requests
	// that reported none. It is an estimate of spend, not a bill, and the two
	// halves are always separable — CostUSD is their sum.
	CostUSD float64 `json:"costUsd"`
	// CostReported is only what providers said, so an estimate can always be
	// told apart from a provider's own number.
	CostReported float64 `json:"costReported"`
	// CostEstimated is the part priced locally from models.yml.
	CostEstimated float64 `json:"costEstimated"`
	// BilledRequests is every request with usage: the denominator. It is
	// above Turns, because a compaction's summarize call is a billed request
	// that is not a turn.
	BilledRequests int `json:"billedRequests"`
	// PricedRequests is the subset that carries a price, reported or
	// estimated. PricedRequests < BilledRequests is the honest zero a gateway
	// produces, and the condition the local price table exists to fix.
	PricedRequests int `json:"pricedRequests"`

	FirstSession time.Time `json:"firstSession,omitempty"`
	LastSession  time.Time `json:"lastSession,omitempty"`
}

// ModelStat is one model's share of the scan.
type ModelStat struct {
	Model string `json:"model"`
	// Sessions counts session FILES this model appeared in, so a subagent
	// session folds into its parent's row without inflating Turns.
	Sessions int   `json:"sessions"`
	Turns    int   `json:"turns"`
	Input    int64 `json:"input"`
	Output   int64 `json:"output"`
	// CacheRead and CacheWrite are the model's own shares of the two
	// prompt-cache buckets. CacheWrite was missing here while the
	// whole-store Totals carried it, so "which model writes the cache"
	// — the expensive half, at 1.25x the input rate — had no answer, and
	// the model's tokens did not add up to its own buckets.
	CacheRead   int64   `json:"cacheRead"`
	CacheWrite  int64   `json:"cacheWrite"`
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

	// pricer is the local price table (Options.Pricer), used only for the
	// requests a provider priced in tokens alone.
	pricer func(string) Pricer
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
	rep.pricer = opts.Pricer
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

// fold merges one file's counters into the report, applying the local price
// table to the requests that came without one.
func (r *Report) fold(m session.SessionMeta, c counters) {
	t := &r.Totals
	t.Sessions++
	if m.TitleSource == "subagent" {
		t.Subagents++
	}
	t.UserMessages += c.UserMessages
	t.InjectedTurns += c.Injected
	t.Turns += c.Turns
	t.ToolCalls += c.ToolCalls
	t.ToolErrors += c.ToolErrors
	t.Input += c.Input
	t.Output += c.Output
	t.CacheRead += c.CacheRead
	t.CacheWrite += c.CacheWrite
	t.TotalTokens += c.TotalTokens
	t.CostUSD += c.CostUSD
	t.CostReported += c.CostUSD
	t.PricedRequests += c.PricedTurns
	t.BilledRequests += c.PricedTurns + unpricedRequests(c.Unpriced)
	// A price table only ever fills a gap: c.CostUSD above is what providers
	// said and is never replaced — the estimate is added to it. It does not
	// reach the per-day rows, because the unpriced buckets are kept per model
	// and not per model per day; ponytail: dayCostUSD under-reports an
	// estimated day. Upgrade path: key Unpriced by day as well as model.
	for model, usd := range r.priceUnpriced(c.Unpriced) {
		t.CostUSD += usd
		t.CostEstimated += usd
		t.PricedRequests += c.Unpriced[model].Turns
		st := r.modelIndex()[model]
		if st == nil {
			st = &ModelStat{Model: model}
			r.modelIndex()[model] = st
		}
		st.CostUSD += usd
	}
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
			st.CacheWrite += mc.CacheWrite
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

// priceUnpriced estimates the requests whose provider reported no cost,
// keyed by model. A model with no entry in the scan's Pricer is returned at
// zero — not omitted — so the caller can add it to the totals without
// double-counting the model's row, and PricedRequests stays honest about how
// much of the history is estimated rather than reported.
func (r *Report) priceUnpriced(unpriced map[string]modelCounters) map[string]float64 {
	out := make(map[string]float64, len(unpriced))
	if r.pricer == nil {
		return out
	}
	for model, b := range unpriced {
		price := r.pricer(model)
		if price == nil {
			continue
		}
		usd := price.USD(b.Input, b.Output, b.CacheRead, b.CacheWrite, b.TotalTokens)
		if usd > 0 {
			out[model] = usd
		}
	}
	return out
}

// unpricedRequests counts the requests collected as unpriced.
func unpricedRequests(unpriced map[string]modelCounters) int {
	n := 0
	for _, b := range unpriced {
		n += b.Turns
	}
	return n
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
	// Summary is a compaction entry's retained-context message. Its usage is
	// what the summarize / handoff-document call cost, so it is token and
	// cost the session paid but which used to land in no counter at all.
	Summary *wireMessage `json:"summary"`
}

type wireMessage struct {
	Role        string      `json:"role"`
	Attribution string      `json:"attribution,omitempty"`
	Model       string      `json:"model"`
	IsError     bool        `json:"isError"`
	ToolName    string      `json:"toolName"`
	Usage       *ai.Usage   `json:"usage"`
	Content     []wireBlock `json:"content"`
}

type wireBlock struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

// addUsage folds one billed request's usage into the totals and its model's
// row, and returns what the caller owes the day rollup (tokens, USD). Every
// figure comes off the persisted per-message Usage: a provider that reports no
// usage contributes nothing, which PricedTurns exposes.
//
// turns says whether this request counts as a TURN in the model's row: a
// compaction summary is a billed request the session paid for, but it is not
// the model advancing the work, so it contributes tokens and cost and not a
// turn. Keeping the flag here is what stops the two callers from drifting.
func (c *counters) addUsage(m *wireMessage, turns bool) (turnTokens, turnCost float64) {
	u := m.Usage
	if u == nil {
		return 0, 0
	}
	name := m.Model
	if name == "" {
		name = "unknown"
	}
	if c.Models == nil {
		c.Models = map[string]modelCounters{}
	}
	mc := c.Models[name]
	if turns {
		mc.Turns++
	}
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
	mc.CacheWrite += u.CacheWrite
	mc.TotalTokens += total
	c.Models[name] = mc
	turnTokens = float64(total)
	if u.Cost != nil {
		// The provider priced it: believed, and never estimated.
		c.CostUSD += u.Cost.Total
		mc.CostUSD += u.Cost.Total
		c.PricedTurns++
		turnCost = u.Cost.Total
	} else if c.Unpriced == nil {
		// Tokens but no price — the exact set a local price table has to
		// fill, kept aside so it can be priced at report time (models.yml is
		// not part of the scan). Collected here rather than at the fold site
		// so a compaction summary with no price is estimated too: it is a
		// billed request like any other, and this is the one place that
		// knows what a request reported.
		c.Unpriced = map[string]modelCounters{}
		c.Unpriced[name] = c.addUnpriced(name, u, total)
	} else {
		c.Unpriced[name] = c.addUnpriced(name, u, total)
	}
	return turnTokens, turnCost
}

// addUnpriced accumulates one unpriced request's buckets. The map's Turns
// field counts REQUESTS here, not turns: a compaction summary is a request
// that is not a turn, and BilledRequests is the denominator that needs both.
func (c *counters) addUnpriced(name string, u *ai.Usage, total int64) modelCounters {
	uc := c.Unpriced[name]
	uc.Input += u.Input
	uc.Output += u.Output
	uc.CacheRead += u.CacheRead
	uc.CacheWrite += u.CacheWrite
	uc.TotalTokens += total
	uc.Turns++
	return uc
}

// addDay books one billed request against its UTC day: tokens and spend move
// the day's totals, and only a TURN moves its turn count — a compaction
// summary is spend without work.
func (c *counters) addDay(e *wireEntry, turn bool, tokens, usd float64) {
	if e.Timestamp.IsZero() {
		return
	}
	if c.Days == nil {
		c.Days = map[string]dayCounters{}
	}
	day := e.Timestamp.UTC().Format("2006-01-02")
	d := c.Days[day]
	if turn {
		d.Turns++
	}
	d.TotalTokens += int64(tokens)
	d.CostUSD += usd
	c.Days[day] = d
}

// counters is the per-file rollup: everything the report needs from one
// session file, small enough to cache thousands of.
//
// CostUSD / PricedTurns here are the PROVIDER-REPORTED subtotal only, and
// Unpriced keeps the buckets of the requests that came without one. A locally
// priced estimate is applied when these fold into a report, never here: it
// depends on models.yml, and a price table edited between two scans must not
// invalidate the cache — rollupVersion is about the file format, not prices.
type counters struct {
	UserMessages int     `json:"userMessages,omitempty"`
	Injected     int     `json:"injected,omitempty"`
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
	// Unpriced are the requests whose provider reported tokens but no cost,
	// by model: the exact set a local price table estimates.
	Unpriced map[string]modelCounters `json:"unpriced,omitempty"`

	Models map[string]modelCounters `json:"models,omitempty"`
	Tools  map[string]toolCounters  `json:"tools,omitempty"`
	Days   map[string]dayCounters   `json:"days,omitempty"`
}

// modelCounters is one model's request buckets: what a per-model row is folded
// from, and what the local price table multiplies (counters.Unpriced is the
// same shape, holding the requests that came without a price).
//
// It is also what rides the on-disk rollup cache, so a field added here
// without bumping rollupVersion is read back as zero from every session
// scanned before the change — the rollupVersion comment names that
// requirement.
type modelCounters struct {
	Turns       int     `json:"turns,omitempty"`
	Input       int64   `json:"input,omitempty"`
	Output      int64   `json:"output,omitempty"`
	CacheRead   int64   `json:"cacheRead,omitempty"`
	CacheWrite  int64   `json:"cacheWrite,omitempty"`
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
	// A compaction entry's summary carries a billed request's usage. It is
	// folded as tokens and cost but is NOT a turn: the turn count is what
	// "how many times the model was asked", and a summarize call asks
	// something different (it compresses, it does not advance the work).
	if e.Type == "compaction" && e.Summary != nil && e.Summary.Usage != nil {
		tokens, usd := c.addUsage(e.Summary, false)
		c.addDay(e, false, tokens, usd)
	}
	if e.Type != "message" || e.Message == nil {
		return
	}
	m := e.Message
	switch m.Role {
	case "user":
		// Typed input carries no attribution or the verbatim "user";
		// anything else ("provider-continuation", "goal-continuation",
		// …) is harness text the user never wrote (#283).
		if m.Attribution == "" || m.Attribution == "user" {
			c.UserMessages++
		} else {
			c.Injected++
		}
	case "assistant":
		c.Turns++
		name := m.Model
		if name == "" {
			name = "unknown"
		}
		turnTokens, turnCost := c.addUsage(m, true)
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
		c.addDay(e, true, turnTokens, turnCost)
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
