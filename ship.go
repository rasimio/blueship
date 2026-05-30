package blueship

import (
	"log/slog"
	"os"
	"sync"

	"github.com/rasimio/blueship/core"
	"github.com/rasimio/blueship/internal/fleet"
)

// fleetAuth bundles Ship-side state populated by runFleet that the A2A
// server's JWT validator depends on. Wrapped in a struct so the A2A
// server can hold a stable pointer at startup time, even though the JWKS
// cache + self_agent_id only become known once Fleet is reachable.
type fleetAuth struct {
	mu          sync.RWMutex
	jwks        *fleet.JWKSCache
	selfAgentID string
}

func (f *fleetAuth) set(jwks *fleet.JWKSCache, selfAgentID string) {
	f.mu.Lock()
	f.jwks = jwks
	f.selfAgentID = selfAgentID
	f.mu.Unlock()
}

func (f *fleetAuth) snapshot() (*fleet.JWKSCache, string) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.jwks, f.selfAgentID
}

// Ship is the main BlueShip runtime instance.
type Ship struct {
	cfg              Config
	modules          []Module
	handlers         map[string]core.AgentHandler // recurring-task handlers, keyed by AgentTask.Handler
	strategyHandlers map[string]core.AgentHandler // strategy executors (direct / structured / delegate), keyed by AgentTask.Strategy
	logger           *slog.Logger
	fleetAuth        *fleetAuth         // populated by runFleet; consumed by A2A server's JWT middleware
	a2aRegistry      *core.ToolRegistry // shared between A2A dispatcher + Fleet identity publish
}

// New creates a new BlueShip instance with the given configuration.
func New(cfg Config) *Ship {
	cfg.ApplyDefaults()

	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}

	return &Ship{
		cfg:       cfg,
		logger:    logger,
		fleetAuth: &fleetAuth{},
	}
}

// RegisterModule adds a module to the BlueShip instance.
func (s *Ship) RegisterModule(m Module) {
	s.modules = append(s.modules, m)
}

// (RegisterGoalHandler retired — agents now register strategy executors
// via RegisterStrategyHandler. The legacy goals table + scheduler were
// removed in Phase B iter3.)

// RegisterAgentHandler registers a named handler for autonomous agent tasks.
// Handlers are dispatched by the agent task scheduler based on the handler field in agent_tasks.
func (s *Ship) RegisterAgentHandler(name string, h core.AgentHandler) {
	if s.handlers == nil {
		s.handlers = make(map[string]core.AgentHandler)
	}
	s.handlers[name] = h
}

// RegisterStrategyHandler registers an executor for a strategy value
// (direct / structured / delegate). The agent_task scheduler falls back
// to strategy-based dispatch when AgentTask.Handler is empty.
func (s *Ship) RegisterStrategyHandler(strategy string, h core.AgentHandler) {
	if s.strategyHandlers == nil {
		s.strategyHandlers = make(map[string]core.AgentHandler)
	}
	s.strategyHandlers[strategy] = h
}
