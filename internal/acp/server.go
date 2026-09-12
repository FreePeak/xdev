package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
)

// AgentVersion is the version reported in the initialize handshake.
var AgentVersion = "0.1.0"

// Handler is the agent behind the protocol: one session per session/new, one
// running turn per session.
type Handler interface {
	// NewSession creates a session rooted at cwd ("" = the agent's own
	// working directory) and returns its id.
	NewSession(ctx context.Context, cwd string) (string, error)
	// Prompt runs one turn, streaming updates through emit. The returned
	// reason is one of the Stop* values ("" means StopEndTurn); an error is
	// reported to the client as a JSON-RPC error unless the turn was
	// cancelled, which always answers StopCancelled.
	Prompt(ctx context.Context, sessionID string, blocks []ContentBlock, emit Emitter) (string, error)
}

// Emitter is the client side of a running turn.
type Emitter interface {
	// Update streams one session/update notification.
	Update(u Update)
	// Permission asks the client to approve one tool call and blocks for the
	// answer. An error means the request could not be answered and must be
	// treated as a refusal.
	Permission(ctx context.Context, req PermissionRequest) (PermissionOutcome, error)
}

// rpcFrame is one JSON-RPC 2.0 message in either direction.
type rpcFrame struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func (f *rpcFrame) idSet() bool { return len(f.ID) > 0 && string(f.ID) != "null" }

// rpcError is a JSON-RPC error object.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("jsonrpc error %d: %s", e.Code, e.Message) }

// Server speaks ACP on one stdio pair. Prompts run off the read loop so a
// session/cancel notification (and a permission answer) can still be read
// while a turn is streaming.
type Server struct {
	h Handler
	r *bufio.Reader
	w io.Writer

	wmu sync.Mutex // serializes frame writes

	mu      sync.Mutex
	nextID  int64 // server-initiated request ids
	pending map[string]chan *rpcFrame
	running map[string]context.CancelFunc
}

// New builds a server over the given streams.
func New(in io.Reader, out io.Writer, h Handler) *Server {
	return &Server{
		h:       h,
		r:       newReader(in), // the frame reader; the cap lives in framing.go
		w:       out,
		pending: map[string]chan *rpcFrame{},
		running: map[string]context.CancelFunc{},
	}
}

// Serve reads frames until the stream ends or a frame cannot be decoded. It
// returns nil on a clean end of stream; running turns are cancelled.
func (s *Server) Serve(ctx context.Context) error {
	defer s.stopAll()
	for {
		body, err := readFrame(s.r)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		var f rpcFrame
		if err := json.Unmarshal(body, &f); err != nil {
			if err := s.respondError(nil, CodeParseError, "parse error: "+err.Error()); err != nil {
				return err
			}
			continue
		}
		switch {
		case f.Method == "":
			// A response to a request this server sent (permission
			// elicitation). Unmatched ids are dropped: the requester is gone.
			s.deliver(&f)
		case !f.idSet():
			if err := s.notify(ctx, &f); err != nil {
				return err
			}
		default:
			if err := s.request(ctx, &f); err != nil {
				return err
			}
		}
	}
}

// request dispatches one client request.
func (s *Server) request(ctx context.Context, f *rpcFrame) error {
	switch f.Method {
	case MethodInitialize:
		var p InitializeParams
		if err := unmarshalParams(f.Params, &p); err != nil {
			return s.respondError(f.ID, CodeInvalidParams, err.Error())
		}
		// A client on another revision is answered with ours; the spec has it
		// disconnect if it cannot work with that version.
		return s.respond(f.ID, InitializeResult{
			ProtocolVersion:   ProtocolVersion,
			AgentCapabilities: AgentCapabilities{},
			AgentInfo:         Implementation{Name: "xdev", Title: "XDev", Version: AgentVersion},
		})
	case MethodNewSession, MethodNewSessionPre10:
		var p NewSessionParams
		if err := unmarshalParams(f.Params, &p); err != nil {
			return s.respondError(f.ID, CodeInvalidParams, err.Error())
		}
		id, err := s.h.NewSession(ctx, p.Cwd)
		if err != nil {
			return s.respondError(f.ID, CodeInternalError, err.Error())
		}
		return s.respond(f.ID, NewSessionResult{SessionID: id})
	case MethodPrompt:
		return s.prompt(ctx, f)
	case MethodCancel, MethodCancelShort:
		// The spec makes cancel a notification; answer the request form
		// rather than failing it, for clients that send it as one.
		if err := s.cancel(f); err != nil {
			return err
		}
		return s.respond(f.ID, struct{}{})
	default:
		return s.respondError(f.ID, CodeMethodNotFound, "method not found: "+f.Method)
	}
}

// notify dispatches one client notification: session/cancel and the two
// spellings of it older clients send. Everything else is ignored, as JSON-RPC
// requires for unknown notifications.
func (s *Server) notify(ctx context.Context, f *rpcFrame) error {
	switch f.Method {
	case MethodCancel, MethodCancelShort:
		return s.cancel(f)
	}
	return nil
}

// prompt starts a turn and answers the request when it ends.
func (s *Server) prompt(ctx context.Context, f *rpcFrame) error {
	var p PromptParams
	if err := unmarshalParams(f.Params, &p); err != nil {
		return s.respondError(f.ID, CodeInvalidParams, err.Error())
	}
	if p.SessionID == "" {
		return s.respondError(f.ID, CodeInvalidParams, "sessionId is required")
	}
	s.mu.Lock()
	if _, busy := s.running[p.SessionID]; busy {
		s.mu.Unlock()
		return s.respondError(f.ID, CodeInvalidRequest, "a turn is already running for this session")
	}
	pctx, cancel := context.WithCancel(ctx)
	s.running[p.SessionID] = cancel
	s.mu.Unlock()

	id := f.ID
	go func() {
		defer cancel()
		stop, err := s.h.Prompt(pctx, p.SessionID, p.Prompt, &emitter{s: s, sessionID: p.SessionID})
		s.mu.Lock()
		delete(s.running, p.SessionID)
		s.mu.Unlock()
		switch {
		case pctx.Err() != nil:
			// A cancelled turn still answers: the client must learn that the
			// prompt it sent is over.
			_ = s.respond(id, PromptResult{StopReason: StopCancelled})
		case err != nil:
			_ = s.respondError(id, CodeInternalError, err.Error())
		default:
			if stop == "" {
				stop = StopEndTurn
			}
			_ = s.respond(id, PromptResult{StopReason: stop})
		}
	}()
	return nil
}

// cancel stops the turn running for the notification's session.
func (s *Server) cancel(f *rpcFrame) error {
	var p CancelParams
	if err := unmarshalParams(f.Params, &p); err != nil {
		// A notification gets no response, even when its params are bad.
		if !f.idSet() {
			return nil
		}
		return s.respondError(f.ID, CodeInvalidParams, err.Error())
	}
	s.mu.Lock()
	cancel := s.running[p.SessionID]
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

// stopAll cancels every running turn (server shutdown).
func (s *Server) stopAll() {
	s.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(s.running))
	for _, cancel := range s.running {
		cancels = append(cancels, cancel)
	}
	s.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

// call sends a request to the client and waits for its response. The read loop
// delivers responses to pending waiters.
func (s *Server) call(ctx context.Context, method string, params any, out any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.nextID++
	// The key is the id exactly as it goes on the wire (a JSON string): the
	// client echoes that back byte for byte.
	rawID := json.RawMessage(strconv.Quote(strconv.FormatInt(s.nextID, 10)))
	key := string(rawID)
	ch := make(chan *rpcFrame, 1)
	s.pending[key] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pending, key)
		s.mu.Unlock()
	}()
	if err := s.write(rpcFrame{JSONRPC: "2.0", ID: rawID, Method: method, Params: raw}); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case f := <-ch:
		if f.Error != nil {
			return f.Error
		}
		if out != nil && len(f.Result) > 0 {
			return json.Unmarshal(f.Result, out)
		}
		return nil
	}
}

func (s *Server) deliver(f *rpcFrame) {
	key := string(f.ID)
	s.mu.Lock()
	ch := s.pending[key]
	s.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- f:
	default:
	}
}

// respond writes a successful response.
func (s *Server) respond(id json.RawMessage, result any) error {
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return s.write(rpcFrame{JSONRPC: "2.0", ID: id, Result: raw})
}

// respondError writes an error response. A nil id (unparseable request) is
// reported as JSON null, as JSON-RPC requires.
func (s *Server) respondError(id json.RawMessage, code int, msg string) error {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return s.write(rpcFrame{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}})
}

func (s *Server) write(f rpcFrame) error {
	body, err := json.Marshal(f)
	if err != nil {
		return err
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	return writeFrame(s.w, body)
}

// emitter is the per-turn Emitter handed to the handler.
type emitter struct {
	s         *Server
	sessionID string
}

func (e *emitter) Update(u Update) {
	raw, err := json.Marshal(struct {
		SessionID string `json:"sessionId"`
		Update    Update `json:"update"`
	}{SessionID: e.sessionID, Update: u})
	if err != nil {
		return
	}
	_ = e.s.write(rpcFrame{JSONRPC: "2.0", Method: MethodUpdate, Params: raw})
}

func (e *emitter) Permission(ctx context.Context, req PermissionRequest) (PermissionOutcome, error) {
	req.SessionID = e.sessionID
	// The response is {"outcome": {…}} — the extra level is ACP's.
	var res struct {
		Outcome PermissionOutcome `json:"outcome"`
	}
	if err := e.s.call(ctx, MethodRequestPermission, req, &res); err != nil {
		return PermissionOutcome{Outcome: OutcomeCancelled}, err
	}
	return res.Outcome, nil
}

// unmarshalParams decodes params, treating an absent body as the zero value.
func unmarshalParams(raw json.RawMessage, v any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("invalid params: %w", err)
	}
	return nil
}
