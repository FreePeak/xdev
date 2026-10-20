package opencode

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// maxBridges bounds the registry. It mirrors cmd/xdev/tabset.go's tabSlots (6)
// and carries the same reason: every bridge holds a live subscription and an
// open store, so the set is a memory ceiling rather than an arbitrary number.
// A registry is not the tabset — a tabset can evict a parked tab, and this
// cannot — so exceeding it is an error the caller can surface rather than a
// silent drop.
const maxBridges = 6

// ErrRegistryFull means every bridge slot is held.
var ErrRegistryFull = fmt.Errorf("opencode: registry full (%d sessions)", maxBridges)

// Bridge is the interface a registry holds, so the registry can be tested
// without a live store.
type bridgeLike interface {
	Handle(Event)
	Cost() float64
	Tokens() TokenUsage
	Unread() int
}

// Registry fans one event stream out to the bridges that care, and keeps the
// roster a status surface renders.
//
// One subscription for every session is the whole point: opencode publishes a
// single global stream, so N bridges on one Stream is N connections otherwise.
type Registry struct {
	mu      sync.RWMutex
	bridges map[string]*Bridge
	order   []string // roster order, most-recently-active first
	client  *Client
	stream  *Stream
}

// NewRegistry returns an empty registry bound to a client.
func NewRegistry(c *Client) *Registry {
	return &Registry{bridges: map[string]*Bridge{}, client: c}
}

// Add registers a bridge. Adding a session that is already registered returns
// the existing one rather than replacing it, so a re-open of the same session
// does not orphan its stream subscription.
func (r *Registry) Add(b *Bridge) (*Bridge, error) {
	if b == nil || b.Info.ID == "" {
		return nil, fmt.Errorf("opencode: cannot register a bridge with no session id")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.bridges[b.Info.ID]; ok {
		return existing, nil
	}
	if len(r.bridges) >= maxBridges {
		return nil, ErrRegistryFull
	}
	r.bridges[b.Info.ID] = b
	r.order = append([]string{b.Info.ID}, r.order...)
	return b, nil
}

// Get returns the bridge for a session id, or nil.
func (r *Registry) Get(id string) *Bridge {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.bridges[id]
}

// Len reports how many sessions are registered.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.bridges)
}

// Handle routes one event to its bridge. An event for an unregistered session
// is dropped: opencode publishes every session on the one stream, including
// sessions xdev was never asked to show.
func (r *Registry) Handle(e Event) {
	id := e.SessionID()
	if id == "" {
		return
	}
	r.mu.RLock()
	b := r.bridges[id]
	r.mu.RUnlock()
	if b == nil {
		return
	}
	b.Handle(e)
	// Touch the roster: an active session floats to the front.
	r.touch(id)
}

func (r *Registry) touch(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, existing := range r.order {
		if existing == id {
			r.order = append(append([]string{}, r.order[:i]...), r.order[i+1:]...)
			break
		}
	}
	r.order = append([]string{id}, r.order...)
}

// RosterRow is one session's status. Everything here comes from Session.Info
// or session.usage.updated — no message is read to draw a row, which is what
// keeps a roster cheap enough to refresh on a timer.
type RosterRow struct {
	ID     string
	Title  string
	Agent  string
	Model  string
	Cost   float64
	Tokens TokenUsage
	Unread int
	Dir    string
	Idle   time.Duration
}

// Roster returns the status rows, most-recently-active first.
func (r *Registry) Roster(now time.Time) []RosterRow {
	r.mu.RLock()
	ids := append([]string(nil), r.order...)
	byID := make(map[string]*Bridge, len(r.bridges))
	for id, b := range r.bridges {
		byID[id] = b
	}
	r.mu.RUnlock()

	out := make([]RosterRow, 0, len(ids))
	for _, id := range ids {
		b := byID[id]
		if b == nil {
			continue
		}
		model := ""
		if b.Info.Model.ID != "" {
			model = b.Info.Model.ProviderID + "/" + b.Info.Model.ID
			if v := b.Info.Model.Variant; v != "" {
				model += ":" + v
			}
		}
		var idle time.Duration
		if ts := b.Info.Time.Updated; ts > 0 {
			idle = now.Sub(time.UnixMilli(ts))
		}
		out = append(out, RosterRow{
			ID:     b.Info.ID,
			Title:  b.Info.Title,
			Agent:  b.Info.Agent,
			Model:  model,
			Cost:   b.Cost(),
			Tokens: b.Tokens(),
			Unread: b.Unread(),
			Dir:    b.Info.Location.Directory,
			Idle:   idle,
		})
	}
	return out
}

// Run subscribes once and fans events out. It returns when ctx ends or the
// stream cannot be established; a terminal error (bad credentials, a
// non-loopback endpoint) is returned rather than retried forever.
func (r *Registry) Run(ctx context.Context) error {
	if r.client == nil {
		return fmt.Errorf("opencode: registry has no client")
	}
	if r.stream == nil {
		ep, err := r.client.Discover(ctx)
		if err != nil {
			return err
		}
		r.stream = &Stream{Endpoint: ep}
	}
	return r.stream.Run(ctx, r.Handle)
}

// Stream exposes the underlying subscription so a caller can report undecodable
// frame counts or share one stream with something else.
func (r *Registry) Stream() *Stream { return r.stream }

// SortRowsByIdle orders rows least-recently-active first. It exists so a
// status surface has one definition of "stuck" rather than each caller
// inlining a comparison.
func SortRowsByIdle(rows []RosterRow) {
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Idle > rows[j].Idle })
}
