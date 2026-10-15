package chromeext

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Server is the CDP-compatible loopback face: GET /json/list and a websocket
// at /devtools/page/<tabId> that turns CDP command frames into extension
// chrome.debugger.sendCommand calls.
type Server struct {
	bridge *Bridge
	listen string
	ln     net.Listener
	http   *http.Server
	wsBase string

	mu    sync.Mutex
	conns map[*pageConn]struct{}
}

// NewServer binds listen (host:port) and prepares the handler. Serve starts it.
func NewServer(b *Bridge, listen string) (*Server, error) {
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, fmt.Errorf("chromeext: listen %s: %w", listen, err)
	}
	addr := ln.Addr().String()
	// Prefer the configured host in ws URLs so clients see 127.0.0.1 not [::1].
	host := listen
	if h, p, err := net.SplitHostPort(listen); err == nil {
		if h == "" || h == "0.0.0.0" {
			h = "127.0.0.1"
		}
		host = net.JoinHostPort(h, p)
	} else {
		host = addr
	}
	s := &Server{
		bridge: b,
		listen: listen,
		ln:     ln,
		wsBase: "ws://" + host,
		conns:  map[*pageConn]struct{}{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/json/list", s.handleList)
	mux.HandleFunc("/json", s.handleList)
	mux.HandleFunc("/json/version", s.handleVersion)
	mux.HandleFunc("/devtools/page/", s.handlePageWS)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":        true,
			"connected": b.Connected(),
			"listen":    host,
		})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "xdev chrome native host\nlisten=%s connected=%v\n", host, b.Connected())
	})
	s.http = &http.Server{Handler: mux}
	return s, nil
}

// Addr is the bound address.
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Serve runs until ctx is cancelled or the listener dies.
func (s *Server) Serve(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		_ = s.Close()
	}()
	err := s.http.Serve(s.ln)
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

// Close stops the HTTP server and every open page websocket.
func (s *Server) Close() error {
	s.mu.Lock()
	for c := range s.conns {
		_ = c.close()
	}
	s.conns = map[*pageConn]struct{}{}
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return s.http.Shutdown(ctx)
}

func (s *Server) handleVersion(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"Browser":          "xdev-chrome-bridge",
		"Protocol-Version": "1.3",
		"User-Agent":       "xdev",
		"WebKit-Version":   "0",
	})
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	targets, err := s.bridge.ListTargets(ctx, s.wsBase)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(targets)
}

func (s *Server) handlePageWS(w http.ResponseWriter, r *http.Request) {
	tabID := strings.TrimPrefix(r.URL.Path, "/devtools/page/")
	tabID = strings.Trim(tabID, "/")
	if tabID == "" {
		http.Error(w, "missing tab id", http.StatusBadRequest)
		return
	}
	if !headerHasUpgrade(r) {
		http.Error(w, "websocket upgrade required", http.StatusUpgradeRequired)
		return
	}
	conn, err := upgradeWS(w, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	pc := &pageConn{s: s, tabID: tabID, ws: conn}
	s.mu.Lock()
	s.conns[pc] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.conns, pc)
		s.mu.Unlock()
		_ = pc.close()
	}()
	pc.serve(r.Context())
}

func headerHasUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

// pageConn is one CDP client session for a single tab.
type pageConn struct {
	s     *Server
	tabID string
	ws    *wsServerConn
}

func (c *pageConn) close() error {
	if c.ws == nil {
		return nil
	}
	return c.ws.Close()
}

func (c *pageConn) serve(ctx context.Context) {
	for {
		data, err := c.ws.ReadMessage()
		if err != nil {
			return
		}
		var req struct {
			ID     int64           `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(data, &req); err != nil {
			_ = c.writeErr(0, -32700, "parse error")
			continue
		}
		if req.Method == "" {
			continue
		}
		// Session hygiene the extension does not need.
		switch req.Method {
		case "Runtime.enable", "Page.enable", "DOM.enable", "Network.enable",
			"Target.setDiscoverTargets", "Target.setAutoAttach",
			"Debugger.enable", "Log.enable":
			_ = c.writeResult(req.ID, map[string]any{})
			continue
		}
		callCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		result, err := c.s.bridge.CDP(callCtx, c.tabID, req.Method, req.Params)
		cancel()
		if err != nil {
			_ = c.writeErr(req.ID, -32000, err.Error())
			continue
		}
		_ = c.writeRawResult(req.ID, result)
	}
}

func (c *pageConn) writeResult(id int64, result any) error {
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return c.writeRawResult(id, raw)
}

func (c *pageConn) writeRawResult(id int64, result json.RawMessage) error {
	if len(result) == 0 {
		result = json.RawMessage(`{}`)
	}
	msg, err := json.Marshal(struct {
		ID     int64           `json:"id"`
		Result json.RawMessage `json:"result"`
	}{ID: id, Result: result})
	if err != nil {
		return err
	}
	return c.ws.WriteMessage(msg)
}

func (c *pageConn) writeErr(id int64, code int, message string) error {
	type errBody struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	msg, err := json.Marshal(struct {
		ID    int64   `json:"id"`
		Error errBody `json:"error"`
	}{ID: id, Error: errBody{Code: code, Message: message}})
	if err != nil {
		return err
	}
	return c.ws.WriteMessage(msg)
}
