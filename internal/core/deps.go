package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"
	"github.com/redis/go-redis/v9"
)

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
	ModelStore ModelConfigQuerier

	// RoleTools maps model roles to allowed tool names (nil = all tools).
	RoleTools RoleToolQuerier

	// Stores provide access to ship DB data without modules querying ship DB directly.
	Prompts       PromptStore      // file-backed prompt store rooted at Config.Prompts
	Users         UserStore        // user_profiles table (nil = not available)
	Sessions      SessionQuerier   // chat_messages/chat_sessions (nil = not available)
	UsageRecorder LLMUsageRecorder // llm_usage writer for direct host-side LLM calls

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
	// transport-local: the current LLM call still sees the inbound payload,
	// but later replies cannot replay the bytes and the cabinet won't surface
	// a chip for anything that didn't arrive through its own upload path.
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
	// on every Telegram update. Hosts return ErrTelegramChatUnpaired to
	// indicate "no link" so the gateway can run the unpaired-chat policy
	// (platform greet vs user-bot silence). Mirrors the field on
	// Config.Gateway; ship.go copies it across at InitDeps time.
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

	// SendToUserOnce is the receipt-returning, single-attempt sibling used by
	// keyed task-program notifications. Implementations must issue at most one
	// provider request: an ambiguous timeout is returned to the journal instead
	// of being retried and risking a duplicate reminder.
	SendToUserOnce func(ctx context.Context, userID uuid.UUID, text string) (TaskNotificationReceipt, error)

	// SendConversationMessage is the dialogue-aware path for a background tool
	// that speaks directly to the current user. Unlike raw SendToUser, the
	// implementation serializes with chat turns and persists the assistant
	// message into the shared conversation. Nil falls back to SendToUser for
	// framework hosts without a gateway coordinator.
	SendConversationMessage func(ctx context.Context, userID uuid.UUID, text string) error

	// DraftAutonomousTurn asks the live chat cortex for an optional,
	// prompt-only assistant turn. It is wired after Gateway construction, so
	// agent handlers must read it from AgentDeps at execution time.
	DraftAutonomousTurn AutonomousTurnDrafter

	// FinalizeAutonomousNotification durably confirms a provider-acknowledged
	// autonomous turn and projects it into the shared dialogue. Gateway calls
	// it while holding the pair-scoped turn lock, before any later Cortex turn
	// may read that dialogue. Nil leaves confirmation to the task scheduler.
	FinalizeAutonomousNotification func(
		ctx context.Context,
		attemptID uuid.UUID,
		receipt TaskNotificationReceipt,
	) error

	// EnsureAutonomousHistory repairs every confirmed, unprojected autonomous
	// turn for one exact live conversation. Gateway calls it under the same
	// pair-scoped turn lock before building Cortex context. This is deliberately
	// a Deps-only runtime seam: it coordinates framework stores and is not host
	// configuration.
	EnsureAutonomousHistory func(
		ctx context.Context,
		userID, soulID uuid.UUID,
		sessionID string,
	) error

	// SendToUserAttachment is the file sibling of SendToUser: it ships a
	// CDN-resolved attachment (PDF / image / text) out the user's paired
	// bot. Lets the agent-task notify path deliver `[attached: UUID]`
	// markers as real files (a research task's PDF report), not raw text.
	// Nil = host hasn't wired it; caller skips file delivery.
	SendToUserAttachment func(ctx context.Context, userID uuid.UUID, rec AttachmentRecord, data []byte) error

	// BotOnboarding drives Telegram-native account creation for fresh
	// users. The gateway invokes it on /start from a chat that has no
	// vaelum.user_identities row; the host runs the FSM (GetState +
	// AdvanceStep) and finalises with CreateAccount in a single tx.
	// Nil disables the inline path — the gateway falls back to
	// replyUnpaired (platform greet / user-bot silence). Mirrors the
	// Config.Gateway field; ship.go copies it across at InitDeps time.
	BotOnboarding BotOnboarding
	DeeplinkLogin DeeplinkLoginApprover
	DeeplinkLink  DeeplinkLinker

	// AuthorizeExecution mirrors Config.AuthorizeExecution. Gateway and
	// scheduler call it at execution boundaries, including cached users.
	AuthorizeExecution ExecutionAuthorizer

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
		Config:                         d.Config,
		Logger:                         d.Logger,
		Redis:                          d.Redis,
		UserID:                         userID,
		ChatID:                         chatID,
		IsOwner:                        isOwner,
		Embedder:                       d.Embedder,
		LLM:                            d.LLM,
		Sender:                         d.Sender,
		ModelStore:                     d.ModelStore,
		RoleTools:                      d.RoleTools,
		Prompts:                        d.Prompts,
		Users:                          d.Users,
		Sessions:                       d.Sessions,
		UsageRecorder:                  d.UsageRecorder,
		ContextInjector:                d.ContextInjector,
		ReflexPreparer:                 d.ReflexPreparer,
		RuleEngine:                     d.RuleEngine,
		MessageEncoder:                 d.MessageEncoder,
		TurnCompletedHook:              d.TurnCompletedHook,
		AgentIterationCompletedHook:    d.AgentIterationCompletedHook,
		ResolveSoul:                    d.ResolveSoul,
		ResolveTelegramChat:            d.ResolveTelegramChat,
		SendToUser:                     d.SendToUser,
		SendToUserOnce:                 d.SendToUserOnce,
		SendConversationMessage:        d.SendConversationMessage,
		DraftAutonomousTurn:            d.DraftAutonomousTurn,
		FinalizeAutonomousNotification: d.FinalizeAutonomousNotification,
		EnsureAutonomousHistory:        d.EnsureAutonomousHistory,
		SendToUserAttachment:           d.SendToUserAttachment,
		BotOnboarding:                  d.BotOnboarding,
		DeeplinkLogin:                  d.DeeplinkLogin,
		DeeplinkLink:                   d.DeeplinkLink,
		AuthorizeExecution:             d.AuthorizeExecution,
		pool:                           d.pool,
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
	logger  *slog.Logger
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

	db, err := p.connect(module, dsn)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", module, err)
	}
	// Pool sized for the agent-task worker-pool (up to maxConcurrentTasks
	// back-to-back loops) running alongside live chat + heartbeat + saver,
	// which all share this module pool. At 5 it starved: a task's BeginTx for
	// the assistant-message append waited past its deadline ("begin tx:
	// context deadline exceeded") under concurrency. Postgres max_connections
	// is 100 with ~22 in use, so 20/module is comfortable headroom.
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(8)
	db.SetConnMaxLifetime(30 * time.Minute)
	p.dbs[module] = db
	return db, nil
}

const (
	dbConnectRetryTimeout = 90 * time.Second
	dbConnectRetryDelay   = time.Second
)

func (p *dbPool) connect(module, dsn string) (*sqlx.DB, error) {
	started := time.Now()
	attempt := 0
	for {
		attempt++
		db, err := sqlx.Connect("postgres", dsn)
		if err == nil {
			if attempt > 1 && p.logger != nil {
				p.logger.Info("db connect recovered",
					"module", module,
					"attempts", attempt,
					"elapsed", time.Since(started).String(),
				)
			}
			return db, nil
		}
		if !isRetryableDBConnectError(err) || time.Since(started) >= dbConnectRetryTimeout {
			return nil, err
		}
		if attempt == 1 && p.logger != nil {
			p.logger.Warn("db connect failed; retrying",
				"module", module,
				"timeout", dbConnectRetryTimeout.String(),
				"error", err,
			)
		}
		time.Sleep(dbConnectRetryDelay)
	}
}

func isRetryableDBConnectError(err error) bool {
	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		switch string(pqErr.Code) {
		case "57P03", // cannot_connect_now: database system is starting up
			"57P01", // admin_shutdown
			"08000", // connection_exception
			"08001", // sqlclient_unable_to_establish_sqlconnection
			"08006", // connection_failure
			"53300": // too_many_connections
			return true
		default:
			return false
		}
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "server closed the connection unexpectedly") ||
		strings.Contains(msg, "database system is starting up")
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
		Config:             cfg,
		Logger:             logger,
		Redis:              rdb,
		Embedder:           cfg.Embedder,
		LLM:                cfg.LLM,
		Sender:             cfg.Sender,
		AuthorizeExecution: cfg.AuthorizeExecution,
		pool: &dbPool{
			dbs:     make(map[string]*sqlx.DB),
			dsn:     cfg.DB,
			schemas: schemas,
			logger:  logger,
		},
	}, nil
}
