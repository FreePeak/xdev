package memory

// Store is the memory backend seam (M12 #44). The local markdown backend
// (*Backend), the mnemopi SQLite backend, and the remote hindsight backend
// all satisfy it, so the prompt injection, the learn tool, and the memory://
// read seam are wired once against this interface and the backend is picked
// by settings at construction time.
//
// The method set is deliberately the smallest common denominator: the
// backend-specific surfaces (polyphonic recall, the retain queue, reflection)
// stay on the concrete types and are only reachable through the tools that
// own them.
type Store interface {
	// Off reports whether the backend is disabled. Every method below is
	// off-safe: a nil or unconfigured backend pays nothing.
	Off() bool
	// Ensure creates the backing storage (first use).
	Ensure() error
	// Summary is the injected guidance text (capped; may be empty).
	Summary() string
	// GuidanceBlock wraps Summary in the "# Memory Guidance" shape omp
	// injects, or "" when there is nothing to inject.
	GuidanceBlock() string
	// SaveLesson stores one lesson with its optional context.
	SaveLesson(text, context string) error
	// Read resolves a memory:// URL to text.
	Read(uri string) (string, error)
	// WriteSummary replaces the consolidated summary.
	WriteSummary(text string) error
	// Clear drops the stored memory.
	Clear() error
	// Stats reports a human-readable backend summary (/memory stats).
	Stats() string
	// Paths exposes the two backing locations (/memory view, diagnostics).
	Paths() (summary, lessons string)
}

// Every shipped local backend must satisfy the seam; the compile-time
// assertions make a drifting method set a build failure, not a wiring
// surprise at startup.
var (
	_ Store = (*Backend)(nil)
	_ Store = (*Mnemopi)(nil)
)
