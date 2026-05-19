package core

import (
	"fmt"

	"github.com/google/uuid"
)

// soulID is the tenant identity of the running ship. Set once at startup
// via SetSoulID. Used by writers that INSERT into tenant-bound tables
// (memory, embeddings, vad_states, digest_runs, chat_*, agent_task*,
// a2a_*, audit_log). Phase A holds a single value for the whole
// process; Phase B will replace this singleton with per-request
// resolution via Deps.SoulID + ctx routing.
var soulID uuid.UUID

// SoulID returns the running ship's tenant identity. Panics if
// SetSoulID has not been called — writers must never run before
// startup wiring is complete, and a silent uuid.Nil INSERT would only
// be caught downstream by a NOT NULL violation. Failing here surfaces
// the wiring bug at its source.
func SoulID() uuid.UUID {
	if soulID == uuid.Nil {
		panic("blueship: SoulID() called before SetSoulID")
	}
	return soulID
}

// SetSoulID initialises the ship's tenant identity. One-shot: a
// second call panics so that misuse surfaces at startup, not later via
// a silently-overwritten value. id must be non-Nil.
func SetSoulID(id uuid.UUID) {
	if id == uuid.Nil {
		panic("blueship: SetSoulID called with uuid.Nil")
	}
	if soulID != uuid.Nil {
		panic(fmt.Sprintf("blueship: SetSoulID called twice (existing=%s, new=%s)", soulID, id))
	}
	soulID = id
}
