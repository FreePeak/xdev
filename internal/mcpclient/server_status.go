package mcpclient

import (
	"context"
	"fmt"
	"time"
)

// ServerStatus is the reachability state one server can be in.
type ServerStatus struct {
	Name      string // short label for the row ("server-name")
	State     string // "online" | "unreachable" | "disabled" | "error"
	Transport string // "stdio" | "http"
}

// ServerHealthProbe probes every configured server's health endpoint
// (or PATH lookup for stdio) and returns a row per server. The config
// is the source of truth for enablement; reachability is a separate
// probe, so a server that is enabled but unreachable still shows
// "unreachable" rather than being silently dropped. The context bounds
// the whole sweep; if it expires mid-sweep the probe returns its
// partial results with the last error.
func ServerHealthProbe(ctx context.Context, cfg *Config) ([]ServerStatus, error) {
	if cfg == nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	names := make([]string, 0, len(cfg.Servers))
	for n := range cfg.Servers {
		names = append(names, n)
	}

	results := make([]ServerStatus, 0, len(cfg.Servers))
	var lastErr error
	for _, name := range names {
		select {
		case <-ctx.Done():
			return results, ctx.Err()
		default:
		}

		sc := cfg.Servers[name]
		if sc == nil {
			continue
		}
		st := ServerStatus{Name: name, Transport: "stdio"}
		if sc.Enabled != nil && !*sc.Enabled {
			st.State = "disabled"
		} else if sc.Disabled {
			st.State = "disabled"
		} else {
			ok, err := sc.IsHealthy()
			if err != nil {
				lastErr = fmt.Errorf("probe %s: %w", name, err)
				st.State = "error"
			} else if !ok {
				st.State = "unreachable"
			} else {
				st.State = "online"
			}
		}
		if sc.URL != "" {
			st.Transport = "http"
		}
		results = append(results, st)
	}
	return results, lastErr
}