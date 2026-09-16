// TUI wiring for /connect: the catalog rendered as picker rows. The listing and
// the file write live in internal/config/connect.go; this file only speaks the
// two dialects to each other.
package main

import (
	"fmt"
	"sort"

	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/tui"
)

// connectPickerItems renders the catalog as picker rows, ordered by what the
// user can act on right now: a credential already in hand first (one Enter and
// it is usable), then rows models.yml declares, then the subscription hosts,
// then everything else.
func connectPickerItems(cfg *config.Config) func() []tui.PickerItem {
	return func() []tui.PickerItem {
		opts := config.ConnectOptions(cfg, connectStore())
		// Sort the rows, not the labels: the picker draws a Section header
		// above the first row of each group, so grouping is an ordering
		// property. Stable, and by name inside a group, so the list holds
		// still while the user types.
		sort.SliceStable(opts, func(i, j int) bool {
			if ri, rj := connectRank(opts[i]), connectRank(opts[j]); ri != rj {
				return ri < rj
			}
			return opts[i].Name < opts[j].Name
		})
		items := make([]tui.PickerItem, 0, len(opts))
		for _, o := range opts {
			detail := fmt.Sprintf("%d models · %s", o.Models, o.API)
			if o.Env != "" {
				detail += " · $" + o.Env
			}
			if o.Ready {
				detail += " · " + o.ReadyFrom
			}
			items = append(items, tui.PickerItem{
				Label: o.Name, Detail: detail, Value: o.Name, Section: connectSection(o),
			})
		}
		return items
	}
}

// connectRank orders the groups; connectSection names them. Together they
// answer the question the list is scanned for, in order: can I use it now, is it
// already here, is it a flat subscription, or is it one more host to get a key
// for.
func connectRank(o config.ConnectOption) int {
	switch {
	case o.Ready:
		return 0
	case o.Connected:
		return 1
	case o.Plan:
		return 2
	default:
		return 3
	}
}

func connectSection(o config.ConnectOption) string {
	switch connectRank(o) {
	case 0:
		return "credential in hand"
	case 1:
		return "already in models.yml"
	case 2:
		return "subscriptions"
	default:
		return "catalog"
	}
}

// connectStore reads the credential store for the listing's state columns. A
// store that will not load is not a reason to hide the catalog: the rows just
// report no stored credential, and the next real read surfaces the error.
func connectStore() config.CredentialStore {
	store, err := config.LoadCredentials()
	if err != nil {
		return config.CredentialStore{}
	}
	return store
}
