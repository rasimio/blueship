package blueship

import (
	"encoding/json"
	"sync"

	"github.com/rasimio/blueship/internal/core"
)

// --- Module registry adapter ---

// remoteToolReg is a cached RemoteTool registration that the ship applies
// to every fresh user-scoped registry built by the gateway. Populated at
// startup during A2A peer discovery so remote tools appear alongside
// local ones in the cortex.
type remoteToolReg struct {
	Name        string
	Description string
	Schema      json.RawMessage
	Mode        string
	PeerName    string
	Handler     core.ToolHandler
	Source      string // "yaml" or "fleet" — used for selective replacement
}

type moduleRegistry struct {
	modules []Module

	mu          sync.Mutex // guards remoteTools (Bootstrap mutates concurrently)
	remoteTools []remoteToolReg
	// targets receive every remote-tool replace so long-lived registries
	// (e.g. the agent-task scheduler's globalRegistry, built once at boot)
	// stay in sync with Fleet bootstrap pushes that arrive later.
	targets []*ToolRegistry
}

func (r *moduleRegistry) RegisterAllTools(registry *ToolRegistry, d *Deps) {
	for _, m := range r.modules {
		if tp, ok := m.(ToolProvider); ok {
			tp.RegisterTools(registry, d)
		}
	}
	r.mu.Lock()
	tools := append([]remoteToolReg(nil), r.remoteTools...)
	r.mu.Unlock()
	for _, rt := range tools {
		registry.RegisterRemote(rt.Name, rt.Description, rt.Schema, rt.Mode, rt.PeerName, rt.Handler)
	}
}

// AppendRemoteTool is the startup-time path used by config-driven A2A peers.
func (r *moduleRegistry) AppendRemoteTool(rt remoteToolReg) {
	r.mu.Lock()
	r.remoteTools = append(r.remoteTools, rt)
	r.mu.Unlock()
}

// ReplaceFleetRemoteTools atomically swaps every Fleet-derived remote tool
// (rows whose source matches sourceTag) with the supplied snapshot. Tools
// from other sources (legacy yaml peers) are preserved. Each registered
// target also receives a RegisterRemote pass so long-lived registries
// see the freshly-discovered peers without rebuilding from scratch.
func (r *moduleRegistry) ReplaceFleetRemoteTools(sourceTag string, fresh []remoteToolReg) {
	r.mu.Lock()
	kept := r.remoteTools[:0]
	for _, t := range r.remoteTools {
		if t.Source != sourceTag {
			kept = append(kept, t)
		}
	}
	for _, t := range fresh {
		t.Source = sourceTag
		kept = append(kept, t)
	}
	r.remoteTools = kept
	targets := append([]*ToolRegistry(nil), r.targets...)
	r.mu.Unlock()

	// Push current snapshot into every target. RegisterRemote overwrites
	// any local registration with the same name — exactly the priority we
	// want for delegation-style flows where federation is authoritative.
	for _, reg := range targets {
		for _, rt := range fresh {
			reg.RegisterRemote(rt.Name, rt.Description, rt.Schema, rt.Mode, rt.PeerName, rt.Handler)
		}
	}
}

// AddTargetRegistry registers an external registry that should receive
// federated tool updates whenever Bootstrap pushes a fresh snapshot.
// Used by the agent_task scheduler so its long-lived globalRegistry
// picks up peers discovered after boot.
func (r *moduleRegistry) AddTargetRegistry(reg *ToolRegistry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.targets = append(r.targets, reg)
}
