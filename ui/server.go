// Package ui serves the awf visual graph tool: a localhost HTTP server that renders a
// workflow's graph (via the Slice-1 graph package) and overlays run state. Slice 2a is
// read-only and request-driven (static graph + snapshot overlay); the live SSE overlay
// is Slice 2b.
//
// Request flow:
//
//	GET /                     -> embedded SPA (ui/dist)
//	GET /api/runs             -> runs for the loaded workflow (digest-filtered)
//	GET /api/graph?run=<id>   -> graph.Projection (static, or static + snapshot overlay)
//
// projectionFor is the single place a (workflow, run) becomes a Projection; Slice 2b's
// SSE watch loop will call it too.
package ui

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/valbaudo/awf/graph"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/state"
)

// SSE cadence (vars so tests can shorten them). pollInterval drives how often the watch
// loop stats the run log; heartbeat keeps idle connections (and proxies) alive.
var (
	ssePollInterval = 500 * time.Millisecond
	sseHeartbeat    = 15 * time.Second
)

// Listen binds a TCP listener on 127.0.0.1 (loopback only -- no remote exposure, which
// is why the server needs no auth). port 0 picks an ephemeral port.
func Listen(port int) (net.Listener, error) {
	return net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
}

// Server holds the immutable loaded workflow and an overlay cache. The workflow is
// loaded once at startup (editing the file requires a restart -- documented).
type Server struct {
	wf       *ir.Workflow
	digest   string
	stateDir string

	static graph.Projection // computed once; the workflow is immutable for the process

	mu    sync.Mutex
	cache map[string]cachedProjection // keyed by run id
}

type cachedProjection struct {
	size  int64
	mtime int64
	proj  graph.Projection
}

// New builds a Server for an already-loaded workflow. digest is the workflow's content
// digest (used to filter /api/runs); stateDir is the runs/ base.
func New(wf *ir.Workflow, digest, stateDir string) *Server {
	return &Server{
		wf:       wf,
		digest:   digest,
		stateDir: stateDir,
		static:   graph.BuildStatic(wf),
		cache:    map[string]cachedProjection{},
	}
}

// Handler returns the HTTP routes. Exposed (not bound) so tests drive it via httptest.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/graph", s.handleGraph)
	mux.HandleFunc("/api/runs", s.handleRuns)
	mux.HandleFunc("/api/events", s.handleEvents)
	mux.Handle("/", http.FileServerFS(dist()))
	return mux
}

// errNoRun marks a requested run id that has no log on disk -> HTTP 404.
var errNoRun = errors.New("ui: no such run")

// projectionFor returns the projection for the loaded workflow, optionally overlaid with
// a run's state. runID == "" is the static graph. The overlay is cached per run, keyed
// by the log's (size, mtime); a changed log invalidates and re-folds. Returns errNoRun
// if the run id has no log.
func (s *Server) projectionFor(runID string) (graph.Projection, error) {
	if runID == "" {
		return s.static, nil
	}
	logPath := filepath.Join(s.stateDir, "runs", runID, "log")
	info, err := os.Stat(logPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return graph.Projection{}, errNoRun
		}
		return graph.Projection{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.cache[runID]; ok && c.size == info.Size() && c.mtime == info.ModTime().UnixNano() {
		return c.proj, nil
	}

	events, err := state.FoldFile(logPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return graph.Projection{}, errNoRun
		}
		return graph.Projection{}, err
	}
	// Full run projection: static graph + runtime instance nodes/edges + overlay.
	proj, err := graph.BuildWithRun(s.wf, events)
	if err != nil {
		return graph.Projection{}, err
	}
	s.cache[runID] = cachedProjection{size: info.Size(), mtime: info.ModTime().UnixNano(), proj: proj}
	return proj, nil
}

func (s *Server) handleGraph(w http.ResponseWriter, r *http.Request) {
	proj, err := s.projectionFor(r.URL.Query().Get("run"))
	if err != nil {
		if errors.Is(err, errNoRun) {
			http.Error(w, "no such run", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, proj)
}

func (s *Server) handleRuns(w http.ResponseWriter, _ *http.Request) {
	rows, err := listRuns(s.stateDir, s.digest)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"runs": rows})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// handleEvents streams projection updates for a run as Server-Sent Events: one event on
// connect, then one per detected log change (stat-then-fold poll), plus periodic
// heartbeats. The watch loop runs IN this handler goroutine and returns on client
// disconnect (r.Context().Done()), so there is no separate goroutine to leak.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	run := r.URL.Query().Get("run")
	proj, err := s.projectionFor(run)
	if err != nil {
		if errors.Is(err, errNoRun) {
			http.Error(w, "no such run", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")

	writeSSE(w, proj)
	flusher.Flush()
	last, _ := s.statKey(run)

	poll := time.NewTicker(ssePollInterval)
	defer poll.Stop()
	hb := time.NewTicker(sseHeartbeat)
	defer hb.Stop()
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return // client disconnected -> handler returns -> nothing leaks
		case <-poll.C:
			cur, _ := s.statKey(run)
			if cur == last {
				continue
			}
			last = cur
			p, perr := s.projectionFor(run)
			if perr != nil {
				return // run vanished or became unreadable -> end the stream
			}
			writeSSE(w, p)
			flusher.Flush()
		case <-hb.C:
			_, _ = io.WriteString(w, ": hb\n\n")
			flusher.Flush()
		}
	}
}

// statKey returns a change key (size-mtime) for a run's log, and whether it exists.
// Empty run id (static graph) has no log to watch -> ("", false), a stable key.
func (s *Server) statKey(runID string) (string, bool) {
	if runID == "" {
		return "", false
	}
	info, err := os.Stat(filepath.Join(s.stateDir, "runs", runID, "log"))
	if err != nil {
		return "", false
	}
	return fmt.Sprintf("%d-%d", info.Size(), info.ModTime().UnixNano()), true
}

func writeSSE(w io.Writer, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
}
