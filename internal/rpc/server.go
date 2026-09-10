// Package rpc implements the M6 embedder contract: a JSONL-over-stdio
// server that correlates id-tagged commands with responses and streams
// agent events. Reads are sequential; writes are serialized behind a
// mutex so the handler goroutine and the read loop can both emit.
package rpc

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"github.com/FreePeak/xdev/internal/protocol"
)

// Handler executes inbound commands. Prompt always returns immediately:
// the implementation runs the turn on its own goroutine and streams
// events + the final response through the server.
type Handler interface {
	Prompt(id, text string)
	Steer(text string) error
	FollowUp(text string) error
	Abort() error
	NewSession() error
	State() protocol.State
	SetModel(model string) error
}

// Server reads command frames, dispatches to the Handler, and emits
// responses/events.
type Server struct {
	h Handler
	r *bufio.Reader
	w io.Writer

	wmu sync.Mutex
}

// New builds a server over the given streams.
func New(in io.Reader, out io.Writer, h Handler) *Server {
	return &Server{h: h, r: bufio.NewReaderSize(in, protocol.FrameLimit), w: out}
}

// Serve runs the read loop until the input closes or ctx is done. It
// emits the ready frame first, then dispatches each inbound line on a
// bounded goroutine pool: replies must never block the read loop, or a
// client that pipelines commands deadlocks the server (out-pipe full
// while the in-pipe is never drained). Serve waits for in-flight
// dispatches before returning so responses flush.
func (s *Server) Serve(ctx context.Context) error {
	s.writeFrame(protocol.Ready{Protocol: protocol.ProtocolVersion, FrameLimit: protocol.FrameLimit}, protocol.TypeReady, "")
	var wg sync.WaitGroup
	sem := make(chan struct{}, maxInflightDispatch)
	for {
		if err := ctx.Err(); err != nil {
			wg.Wait()
			return err
		}
		line, err := s.readFrame()
		if err == io.EOF {
			wg.Wait()
			return nil
		}
		if err != nil {
			wg.Wait()
			return err
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(line []byte) {
			defer wg.Done()
			defer func() { <-sem }()
			s.dispatch(ctx, line)
		}(line)
	}
}

// maxInflightDispatch bounds concurrent command handling (backpressure
// is a feature, PRD §1.6).
const maxInflightDispatch = 8

// readFrame reads one JSONL line, enforcing the frame limit.
func (s *Server) readFrame() ([]byte, error) {
	var buf []byte
	for {
		chunk, err := s.r.ReadSlice('\n')
		buf = append(buf, chunk...)
		if err == nil {
			break
		}
		if err == bufio.ErrBufferFull {
			if len(buf) > protocol.FrameLimit {
				return nil, fmt.Errorf("rpc: frame exceeds %d bytes", protocol.FrameLimit)
			}
			continue
		}
		if len(buf) > 0 && err == io.EOF {
			return buf, io.EOF
		}
		return nil, err
	}
	if len(buf) > protocol.FrameLimit {
		return nil, fmt.Errorf("rpc: frame exceeds %d bytes", protocol.FrameLimit)
	}
	return buf, nil
}

// inbound is the command envelope: one typed payload per line.
type inbound struct {
	Type  string `json:"type"`
	ID    string `json:"id"`
	Text  string `json:"text"`
	Model string `json:"model"`
}

func (s *Server) dispatch(ctx context.Context, line []byte) {
	var cmd inbound
	if err := json.Unmarshal(line, &cmd); err != nil {
		s.error("", "bad frame: "+err.Error())
		return
	}
	switch cmd.Type {
	case protocol.TypePrompt:
		if cmd.Text == "" {
			s.error(cmd.ID, "prompt: text required")
			return
		}
		s.h.Prompt(cmd.ID, cmd.Text)
	case protocol.TypeSteer:
		s.reply(cmd.ID, s.h.Steer(cmd.Text))
	case protocol.TypeFollowUp:
		s.reply(cmd.ID, s.h.FollowUp(cmd.Text))
	case protocol.TypeAbort:
		s.reply(cmd.ID, s.h.Abort())
	case protocol.TypeNewSession:
		if err := s.h.NewSession(); err != nil {
			s.error(cmd.ID, err.Error())
			return
		}
		st := s.h.State()
		s.writeFrame(protocol.Response{Ok: true, SessionID: st.SessionID}, protocol.TypeResponse, cmd.ID)
	case protocol.TypeSetModel:
		if err := s.h.SetModel(cmd.Model); err != nil {
			s.error(cmd.ID, err.Error())
			return
		}
		s.writeFrame(protocol.Response{Ok: true, Model: cmd.Model}, protocol.TypeResponse, cmd.ID)
	case protocol.TypeState:
		st := s.h.State()
		s.writeFrame(protocol.Response{Ok: true, State: &st}, protocol.TypeResponse, cmd.ID)
	case "":
		s.error(cmd.ID, "missing frame type")
	default:
		s.error(cmd.ID, "unknown frame type "+cmd.Type)
	}
	_ = ctx // reserved: read-loop cancellation
}

func (s *Server) reply(id string, err error) {
	if err != nil {
		s.error(id, err.Error())
		return
	}
	s.writeFrame(protocol.Response{Ok: true}, protocol.TypeResponse, id)
}

func (s *Server) error(id, msg string) {
	s.writeFrame(protocol.Response{Ok: false, Err: msg}, protocol.TypeResponse, id)
}

// Respond emits the terminal response for a prompt run.
func (s *Server) Respond(id string, resp protocol.Response) {
	s.writeFrame(resp, protocol.TypeResponse, id)
}

// SendEvent streams one event frame correlated with the prompt id.
func (s *Server) SendEvent(id string, ev protocol.Event) {
	s.writeFrame(ev, protocol.TypeEvent, id)
}

func (s *Server) writeFrame(payload any, typ, id string) {
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	env := struct {
		Type  string          `json:"type"`
		ID    string          `json:"id,omitempty"`
		Frame json.RawMessage `json:"frame"`
	}{Type: typ, ID: id, Frame: body}
	out, err := json.Marshal(env)
	if err != nil {
		return
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, _ = s.w.Write(out)
	_, _ = s.w.Write([]byte("\n"))
}
