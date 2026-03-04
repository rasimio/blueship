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
		Config:  d.Config,
		Logger:  d.Logger,
		Redis:   d.Redis,
		UserID:  userID,
		ChatID:  chatID,
		IsOwner: isOwner,
		pool:    d.pool,
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
type dbPool struct {
	mu        sync.Mutex
	dbs       map[string]*sqlx.DB
	dsn       string            // base DSN (app database)
	overrides map[string]string // module → custom DSN
}

func (p *dbPool) get(module string) (*sqlx.DB, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if db, ok := p.dbs[module]; ok {
		return db, nil
	}

	dsn := p.dsn
	if override, ok := p.overrides[module]; ok {
		dsn = override
	} else if module != "core" && module != "" {
		// Append module suffix to database name.
		dsn = appendDBSuffix(dsn, module)
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

// appendDBSuffix adds _<suffix> to the dbname in a PostgreSQL DSN.
// Handles URI format: postgres://user:pass@host:port/dbname?params
// and key=value format: host=localhost dbname=mydb ...
func appendDBSuffix(dsn, suffix string) string {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return dsn
		}
		u.Path = strings.TrimPrefix(u.Path, "/") + "_" + suffix
		u.Path = "/" + u.Path
		return u.String()
	}
	// key=value format: replace dbname=X with dbname=X_suffix
	if strings.Contains(dsn, "dbname=") {
		return strings.Replace(dsn, "dbname=", "dbname="+suffix+"_", 1)
	}
	return dsn + " dbname=" + suffix
}

// replaceDBName replaces the database name in a PostgreSQL DSN.
func replaceDBName(dsn, newName string) string {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return dsn
		}
		u.Path = "/" + newName
		return u.String()
	}
	// key=value format
	if strings.Contains(dsn, "dbname=") {
		// Replace existing dbname value (up to next space or end of string)
		parts := strings.SplitN(dsn, "dbname=", 2)
		after := parts[1]
		if idx := strings.IndexByte(after, ' '); idx >= 0 {
			return parts[0] + "dbname=" + newName + after[idx:]
		}
		return parts[0] + "dbname=" + newName
	}
	return dsn + " dbname=" + newName
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

	// Build DSN overrides. "ship" gets its own database if configured,
	// otherwise falls back to the base (app) database.
	overrides := make(map[string]string)
	if cfg.ShipDB != "" {
		overrides["ship"] = replaceDBName(cfg.DB, cfg.ShipDB)
	} else {
		overrides["ship"] = cfg.DB
	}

	return &Deps{
		Config: cfg,
		Logger: logger,
		Redis:  rdb,
		pool: &dbPool{
			dbs:       make(map[string]*sqlx.DB),
			dsn:       cfg.DB,
			overrides: overrides,
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
