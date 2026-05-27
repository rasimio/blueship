package core

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"
	"github.com/redis/go-redis/v9"
)

// chatIDCtxKey is a typed context key carrying the user's transport
// chat id (e.g. "telegram:5452235517") through the agent loop to tool
// handlers, so a tool can identify the originating chat without taking
// a snapshot of Deps. Set by the gateway before dispatching cortex.
type chatIDCtxKey struct{}

// ContextWithChatID returns a copy of ctx that carries the given chat
// id. Empty chat id is a no-op.
func ContextWithChatID(ctx context.Context, chatID string) context.Context {
	if chatID == "" {
		return ctx
	}
	return context.WithValue(ctx, chatIDCtxKey{}, chatID)
}

// ChatIDFromContext returns the chat id stashed via ContextWithChatID,
// or "" when the context wasn't tagged.
func ChatIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(chatIDCtxKey{}).(string)
	return v
}

// soulIDCtxKey is a typed context key carrying the tenant identity of
// the request — which soul this incoming message / agent task / CLI
// invocation belongs to. Resolved at transport boundaries (gateway,
// scheduler, CLI startup), threaded through ctx so every downstream
// repo INSERT can read it without needing soul-specific Deps wiring.
// One arlene runtime hosts N souls concurrently; soul is per-request,
// not per-process.
type soulIDCtxKey struct{}

// WithSoulID returns a copy of ctx carrying the given soul identity.
// Passing uuid.Nil is a no-op (treated as "no soul resolution
// happened yet"). Idempotent: re-setting overwrites cleanly.
func WithSoulID(ctx context.Context, id uuid.UUID) context.Context {
	if id == uuid.Nil {
		return ctx
	}
	return context.WithValue(ctx, soulIDCtxKey{}, id)
}

// SoulIDFromContext returns the soul id stashed via WithSoulID, or
// uuid.Nil when the context was never tagged. A Nil result on a write
// path is a routing bug — but it is surfaced by the NOT NULL / FK
// constraint on the tenant table (a loud, logged error) rather than a
// panic, so a single unwired path can't take the whole daemon down
// from inside a background goroutine. Use SoulIDFromContextOK when the
// caller needs to branch on presence without relying on the constraint.
func SoulIDFromContext(ctx context.Context) uuid.UUID {
	v, _ := ctx.Value(soulIDCtxKey{}).(uuid.UUID)
	return v
}

// SoulIDFromContextOK returns the soul id and a found flag.
func SoulIDFromContextOK(ctx context.Context) (uuid.UUID, bool) {
	v, ok := ctx.Value(soulIDCtxKey{}).(uuid.UUID)
	if !ok || v == uuid.Nil {
		return uuid.Nil, false
	}
	return v, true
}

// Deps holds runtime dependencies available to modules.
type Deps struct {
	Config  *Config
	Logger  *slog.Logger
	Redis   *redis.Client
	UserID  uuid.UUID // resolved per-invocation
	ChatID  string    // raw chat_id string
	IsOwner bool

	// Optional providers (populated from Config during InitDeps).
	Embedder EmbeddingProvider  // nil = embedding features disabled
	LLM      CompletionProvider // nil = LLM features disabled
	Sender   MessageSender      // nil = message sending disabled

	// ModelStore reads model assignments from DB (nil = use Config.Models).
	ModelStore *ModelConfigStore

	// RoleTools maps model roles to allowed tool names (nil = all tools).
	RoleTools *RoleToolStore

	// Stores provide access to ship DB data without modules querying ship DB directly.
	Prompts  PromptStore    // file-backed prompt store rooted at Config.Prompts
	Users    UserStore      // user_profiles table (nil = not available)
	Sessions SessionQuerier // chat_messages/chat_sessions (nil = not available)

	// ContextInjector is called before the first LLM turn to inject per-request context
	// (e.g. memory traces). Returns empty string to skip injection.
	//
	// `priorContext` carries a few preceding chat turns (concatenated, truncated)
	// so the AME query embedding picks up the multi-turn theme, not just the
	// current short message. Pass "" when no prior turns exist.
	ContextInjector func(ctx context.Context, userID, message, priorContext string) string

	// ReflexPreparer returns structured context for the reflex/cortex pipeline.
	// If set and reflex model is configured, gateway uses this instead of ContextInjector.
	// Falls back to ContextInjector if not set.
	//
	// `priorContext` is the same as in ContextInjector: prior-turns thread
	// digest used to enrich the AME embedding query. Reflex LLM still
	// classifies intent against `message` alone; priorContext only affects
	// memory retrieval.
	ReflexPreparer func(ctx context.Context, userID, message, priorContext string) *ReflexContext

	// RuleEngine evaluates structured rule conditions (scope, intent, state, time, user)
	// and returns rules that should be active for the current context.
	// Called after reflex determines intent/strategy. Results injected into cortex guidance.
	RuleEngine func(ctx context.Context, rc RuleContext) []ActiveRule

	// MessageEncoder is called after each user message to extract and save facts.
	// Runs non-blocking in background. Implementations handle their own DB, embeddings, emotions.
	MessageEncoder func(ctx context.Context, userID, message string)

	// AttachmentSink, when set, receives every file that lands on an
	// inbound transport (Telegram photo, Telegram document, cabinet
	// upload). The host implementation owns where the bytes live —
	// typical wiring is a content-addressed disk store + a metadata
	// row in vaelum.chat_attachments — so the cabinet's chat history
	// can show a chip with a download link regardless of which
	// transport originally produced the file. Nil leaves attachments
	// transport-local: the LLM still sees them via chat_messages,
	// but the cabinet won't surface a chip on reload for anything
	// that didn't arrive through the cabinet's own /api/chat path.
	AttachmentSink AttachmentSink

	// TurnCompletedHook is called after the gateway finishes sending an
	// assistant reply for a turn (both batch and streaming paths). The
	// implementation receives the user UUID and session UUID and is
	// expected to dispatch the event to a per-user memory state machine
	// actor (or whatever consumer the embedding application provides).
	// Called in a goroutine so a slow consumer can't stall the response
	// loop. Nil = no consumer registered, hook is skipped.
	TurnCompletedHook func(ctx context.Context, userID, sessionID uuid.UUID)

	// AgentIterationCompletedHook fires after each successful iteration
	// of an agent_task (any strategy: recurring / direct / structured /
	// delegate). Receiver gets the task as-was at handler entry plus the
	// IterationResult (Pause / Done / continue + Output / Progress).
	// Used by hosts to drive per-iteration memory writes (research
	// artifacts → AME) so background loops persist findings without the
	// LLM having to call memory_save itself. Runs in a goroutine. Nil =
	// no consumer registered.
	AgentIterationCompletedHook func(ctx context.Context, task AgentTask, result IterationResult)

	// SelfAgentID returns the Ship's own Fleet-issued agent id, or "" if
	// Fleet hasn't bootstrapped yet. Used by delegate-strategy handlers
	// so the peer can route status callbacks back here.
	SelfAgentID func() string

	// ResolveSoul maps an already-resolved user to the soul that should
	// handle their request. Called at the gateway boundary after user
	// resolution; the resolved soul is threaded through ctx via
	// WithSoulID so every downstream write is tenant-attributed. The
	// embedding application supplies the implementation (membership-
	// graph lookup) — blueship stays generic about how souls are
	// routed. Nil leaves ctx soul-less, a misconfiguration for any
	// tenant-bound write.
	ResolveSoul func(ctx context.Context, userID uuid.UUID) (uuid.UUID, error)

	// ResolveTelegramChat maps an inbound Telegram message (bot id,
	// numeric chat id) to its bound (user, soul). The gateway calls this
	// on every Telegram update AFTER pairing interception. Hosts return
	// ErrTelegramChatUnpaired to indicate "no link" so the gateway can
	// run the unpaired-chat policy (platform greet vs user-bot silence).
	// Mirrors the field on Config.Gateway; ship.go copies it across at
	// InitDeps time.
	ResolveTelegramChat func(ctx context.Context, botID uuid.UUID, tgChatID int64) (userID, soulID uuid.UUID, err error)

	// SendToUser is a per-user Telegram sender that picks the right
	// bot from vaelum.bot_links (multi-bot host pattern). Wired by
	// ship.go after the Gateway is built so the agent-task scheduler
	// can deliver heartbeat/inner-thought Notify through the bot the
	// user is actually talking to — not the legacy Transport.BotToken
	// which is the host owner's private bot. Returns the underlying
	// telegram-API error so callers can recognise 403 "Forbidden" etc.
	// Nil = host hasn't set it up; caller must fall back to legacy
	// deps.Sender.
	SendToUser func(ctx context.Context, userID uuid.UUID, text string) error

	pool *dbPool
}

// DB returns a connection to the module's database (lazy-open, cached).
// Module "core" connects to the base database; others to dbname_<module>.
func (d *Deps) DB(module string) (*sqlx.DB, error) {
	return d.pool.get(module)
}

// ForUser returns a shallow copy of Deps with a different user identity.
// The DB pool and Redis are shared (goroutine-safe).
func (d *Deps) ForUser(userID uuid.UUID, chatID string, isOwner bool) *Deps {
	return &Deps{
		Config:          d.Config,
		Logger:          d.Logger,
		Redis:           d.Redis,
		UserID:          userID,
		ChatID:          chatID,
		IsOwner:         isOwner,
		Embedder:        d.Embedder,
		LLM:             d.LLM,
		Sender:          d.Sender,
		ModelStore:      d.ModelStore,
		RoleTools:       d.RoleTools,
		Prompts:         d.Prompts,
		Users:           d.Users,
		Sessions:        d.Sessions,
		ContextInjector:   d.ContextInjector,
		ReflexPreparer:    d.ReflexPreparer,
		RuleEngine:        d.RuleEngine,
		MessageEncoder:    d.MessageEncoder,
		TurnCompletedHook: d.TurnCompletedHook,
		AgentIterationCompletedHook: d.AgentIterationCompletedHook,
		ResolveSoul:         d.ResolveSoul,
		ResolveTelegramChat: d.ResolveTelegramChat,
		SendToUser:          d.SendToUser,
		pool:                d.pool,
	}
}

// Close releases all database connections and Redis client.
func (d *Deps) Close() {
	d.pool.mu.Lock()
	defer d.pool.mu.Unlock()

	for _, db := range d.pool.dbs {
		db.Close()
	}
	if d.Redis != nil {
		d.Redis.Close()
	}
}

// dbPool holds lazily-opened database connections, safe for concurrent use.
// Each module gets its own connection with a schema-specific search_path.
type dbPool struct {
	mu      sync.Mutex
	dbs     map[string]*sqlx.DB
	dsn     string            // base DSN (single database)
	schemas map[string]string // module → schema name (empty = public)
}

func (p *dbPool) get(module string) (*sqlx.DB, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if db, ok := p.dbs[module]; ok {
		return db, nil
	}

	dsn := p.dsn
	schema := ""
	if s, ok := p.schemas[module]; ok {
		schema = s
	} else if module != "core" && module != "" {
		schema = module
	}

	if schema != "" {
		dsn = withSearchPath(dsn, schema)
	}

	db, err := sqlx.Connect("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", module, err)
	}
	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(30 * time.Minute)
	p.dbs[module] = db
	return db, nil
}

// withSearchPath appends search_path=<schema>,public to a PostgreSQL DSN.
// lib/pq passes unknown connection parameters as SET commands on connect,
// so every pooled connection automatically gets the correct search_path.
func withSearchPath(dsn, schema string) string {
	sp := schema + ",public"
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return dsn
		}
		q := u.Query()
		q.Set("search_path", sp)
		u.RawQuery = q.Encode()
		return u.String()
	}
	// key=value format
	return dsn + " search_path=" + sp
}

// initDeps creates Deps from a Config. Used internally by Ship.Run().
func InitDeps(cfg *Config, logger *slog.Logger) (*Deps, error) {
	var rdb *redis.Client
	if cfg.Redis != "" {
		rdb = redis.NewClient(&redis.Options{
			Addr:        cfg.Redis,
			DialTimeout: 5 * time.Second,
		})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := rdb.Ping(ctx).Err(); err != nil {
			logger.Warn("redis not available, continuing without cache", "error", err)
		}
	}

	// Build schema map. "ship" gets its own schema if configured.
	schemas := make(map[string]string)
	if cfg.ShipSchema != "" {
		schemas["ship"] = cfg.ShipSchema
	}

	return &Deps{
		Config:   cfg,
		Logger:   logger,
		Redis:    rdb,
		Embedder: cfg.Embedder,
		LLM:      cfg.LLM,
		Sender:   cfg.Sender,
		pool: &dbPool{
			dbs:     make(map[string]*sqlx.DB),
			dsn:     cfg.DB,
			schemas: schemas,
		},
	}, nil
}

// --- CLI output helpers ---

// Response is the standard JSON output format for CLI commands.
type Response struct {
	Success  bool        `json:"success"`
	Data     interface{} `json:"data,omitempty"`
	Error    string      `json:"error,omitempty"`
	Fallback interface{} `json:"fallback,omitempty"`
	Cached   bool        `json:"cached"`
	CachedAt *time.Time  `json:"cached_at,omitempty"`
}

// OK writes a success response to stdout.
func OK(data interface{}) {
	r := Response{Success: true, Data: data, Cached: false}
	writeResponse(r)
}

// Fail writes an error response to stdout and exits.
func Fail(err string) {
	r := Response{Success: false, Error: err, Cached: false}
	writeResponse(r)
	os.Exit(1)
}

func writeResponse(r Response) {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "output marshal error: %v\n", err)
		fmt.Printf(`{"success":false,"error":"internal marshal error","cached":false}`)
		os.Exit(1)
	}
	fmt.Println(string(data))
}
