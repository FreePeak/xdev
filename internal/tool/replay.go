package tool

// Replayer is the optional Tool interface for durability: it declares whether
// running this tool again after an interrupted turn is harmless.
//
// The rebuild path already reports an unanswered call to the model —
// session.UnansweredToolCallNotice — but it cannot say whether re-running is
// free, so it asks the model to judge. That judgement belongs to the tool's
// author, not to the model: `read` is free to repeat, `bash` is not, and a
// model told only "run it again if you still need it" has to guess which it
// just called.
//
// A tool that implements Replayer answers once, here. A tool that implements
// nothing is UNSAFE, which is the direction that costs one redundant read
// rather than one duplicated side effect.
type Replayer interface {
	// ReplaySafe reports whether Execute may be called again with the same
	// arguments and produce no worse an outcome than the interrupted
	// attempt did. Reads qualify even though the file may have changed since:
	// fresher content is not a worse answer.
	ReplaySafe() bool
}

// ReplaySafe reports whether the named tool is registered and declares itself
// safe to re-run after an interrupted turn.
//
// An unregistered name is UNSAFE, and nil is unsafe for everything: a session
// file may name a tool this process does not have (a --tools filter, a remote
// transcript, a renamed tool), and assuming an uninspectable tool is harmless
// is precisely the failure that duplicates a side effect.
func (r *Registry) ReplaySafe(name string) bool {
	if r == nil || name == "" {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	if !ok {
		return false
	}
	rp, ok := t.(Replayer)
	return ok && rp.ReplaySafe()
}
