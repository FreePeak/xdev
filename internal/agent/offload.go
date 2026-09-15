package agent

// ArtifactOffloader is the artifact-offload seam for oversized tool
// results (#283 RCA §4: 1.2 % of results exceed 16 KB and up to 150 KB
// of them sit verbatim in context — the heaviest resident bytes a step
// carries). The loop asks the offloader at the one choke point every
// tool result passes through (runOneTool, after extensions and rule
// notes have finalized the text): the full output goes to durable
// storage, the conversation keeps a small stub pointing at the handle.
//
// #115 owns the backend (blob-store retention + the model-facing
// re-read, per the handle contract in that issue); until it lands,
// Agent.Offload stays nil and results are retained verbatim — today's
// behavior, unchanged.
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
// to the offloader. 16 KiB matches the #115 spill threshold.
const OffloadThresholdBytes = 16 << 10
