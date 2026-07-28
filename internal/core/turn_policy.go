package core

import "context"

// TurnPolicyMode is the host-requested rollout state for a per-turn policy.
// BlueShip only defines the lifecycle; hosts own the meaning of each policy
// and the domain-specific intent classification that activates it.
type TurnPolicyMode string

const (
	TurnPolicyOff    TurnPolicyMode = "off"
	TurnPolicyShadow TurnPolicyMode = "shadow"
	TurnPolicyCanary TurnPolicyMode = "canary"
	TurnPolicyOn     TurnPolicyMode = "on"
)

// TurnPolicyRequest contains only generic facts available before Cortex
// preparation. AvailableTools is already intersected with the role registry
// and the current soul policy.
type TurnPolicyRequest struct {
	Role           string
	UserID         string
	SoulID         string
	UserText       string
	BaseTools      []string
	AvailableTools []string
	Ephemeral      bool
}

// TurnPolicy is the host's atomic decision for one turn.
//
// Tools == nil preserves normal ToolSelector behavior. A non-nil Tools slice
// replaces that selection. DeniedTools is a hard runtime denylist applied
// after every role/selector/toolbox expansion.
type TurnPolicy struct {
	RequestedMode TurnPolicyMode
	EffectiveMode TurnPolicyMode
	Tools         []string
	DeniedTools   []string
	Strict        bool
	// RequireCortex bypasses optional fast answer tiers without activating
	// tools, guidance, or strict mode. Hosts use it when the full operational
	// contract is required even while a feature rollout is off or shadowing.
	RequireCortex bool
	Guidance      string
	PreActions    []ToolAction
	// ObserverActions run best-effort in a detached goroutine and can never
	// alter the prompt, toolset, or response. Intended for shadow rollout.
	ObserverActions        []ToolAction
	SuppressToolDirectives bool
	Flags                  []string
	Diagnostics            map[string]string
	Source                 string
	UnavailableReason      string
}

// Active reports whether the policy may affect the Cortex turn.
func (p TurnPolicy) Active() bool {
	return p.EffectiveMode == TurnPolicyCanary || p.EffectiveMode == TurnPolicyOn
}

// TurnPolicyResolver lets a host resolve mode, evidence pre-actions, prompt
// guidance, and concrete tools once from the same turn facts.
type TurnPolicyResolver func(context.Context, TurnPolicyRequest) TurnPolicy
