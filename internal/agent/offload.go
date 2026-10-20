package agent

// ArtifactOffloader is the artifact-offload seam for oversized tool
// results (#283 RCA §4: 1.2 % of results exceed 16 KB and up to 150 KB
// of them sit verbatim in context — the heaviest resident bytes a step
// carries). The loop asks the offloader at the one choke point every
// tool result passes through (runOneTool, after extensions and rule
// notes have finalized the text): the full output goes to durable
// storage, the conversation keeps a small stub pointing at the handle.
//
// The backend ships: FileOffloader (offload_file.go) writes the output to
// a file under the xdev data dir and returns opencode's stub — the head of
// the output plus a marker naming the file, the same shape opencode's
// tool-output.ts produces. An agent built without one (ag.Offload nil)
// keeps results verbatim, as before.
type ArtifactOffloader interface {
	// Offload persists text out-of-band and returns the replacement the
	// conversation should carry. ok=false leaves the result untouched
	// (the offloader declined this one — no store bound, size policy).
	// An error means the durable write FAILED: callers log it and keep
	// the bytes verbatim, because a marker claiming "full output at
	// <handle>" must be a true claim about storage, not a guess.
	Offload(toolName, callID, text string) (stub string, ok bool, err error)
}

// OffloadThresholdBytes is the seam's size gate: results at or below it
// are the cheap majority (store-wide median 952 B) and are never handed
// to the offloader. 50 KiB matches opencode's tool-output.ts MAX_BYTES,
// and the shipped backend's own limit — one number, one meaning: a result
// over it is offloaded, a result under it stays in the conversation.
const OffloadThresholdBytes = 50 << 10
