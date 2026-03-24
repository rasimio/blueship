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
	Prompts  PromptStore    // system_prompts table (nil = not available)
	Users    UserStore      // user_profiles table (nil = not available)
	Sessions SessionQuerier // chat_messages/chat_sessions (nil = not available)

	// ContextInjector is called before the first LLM turn to inject per-request context
	// (e.g. memory traces). Returns empty string to skip injection.
	ContextInjector func(ctx context.Context, userID, message string) string

	// ReflexPreparer returns structured context for the reflex/cortex pipeline.
	// If set and reflex model is configured, gateway uses this instead of ContextInjector.
	// Falls back to ContextInjector if not set.
	ReflexPreparer func(ctx context.Context, userID, message string) *ReflexContext

	// MessageEncoder is called after each user message to extract and save facts.
	// Runs non-blocking in background. Implementations handle their own DB, embeddings, emotions.
	MessageEncoder func(ctx context.Context, userID, message string)

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
		ContextInjector: d.ContextInjector,
		ReflexPreparer:  d.ReflexPreparer,
		pool:            d.pool,
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
