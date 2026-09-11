// Package database opens and migrates the goal-engine's PostgreSQL databases.
package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/golang-migrate/migrate/v4"
	migratepostgres "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file" // file:// migration source
	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq" // postgres driver
)

// Pool defaults. They are deliberately modest: this service does a small amount
// of work on a timer, and its read pools point at other products' databases.
const (
	defaultMaxOpenConns    = 10
	defaultMaxIdleConns    = 5
	defaultConnMaxLifetime = 30 * time.Minute
	defaultConnMaxIdleTime = 5 * time.Minute
	defaultPingTimeout     = 10 * time.Second
)

// Options tunes a connection pool.
type Options struct {
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	ConnMaxIdleTime time.Duration
	PingTimeout     time.Duration
}

// Option mutates Options.
type Option func(*Options)

// WithMaxOpenConns caps the pool size.
func WithMaxOpenConns(n int) Option {
	return func(o *Options) {
		if n > 0 {
			o.MaxOpenConns = n
			if o.MaxIdleConns > n {
				o.MaxIdleConns = n
			}
		}
	}
}

// WithPingTimeout bounds the startup connectivity check.
func WithPingTimeout(d time.Duration) Option {
	return func(o *Options) {
		if d > 0 {
			o.PingTimeout = d
		}
	}
}

// Connect opens a pool and verifies it can reach the server. A service that
// cannot reach its database should fail at startup, not on its first decision.
func Connect(ctx context.Context, dsn string, opts ...Option) (*sqlx.DB, error) {
	if dsn == "" {
		return nil, errors.New("database: dsn is empty")
	}

	options := Options{
		MaxOpenConns:    defaultMaxOpenConns,
		MaxIdleConns:    defaultMaxIdleConns,
		ConnMaxLifetime: defaultConnMaxLifetime,
		ConnMaxIdleTime: defaultConnMaxIdleTime,
		PingTimeout:     defaultPingTimeout,
	}
	for _, opt := range opts {
		opt(&options)
	}

	db, err := sqlx.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("database: open: %w", err)
	}
	db.SetMaxOpenConns(options.MaxOpenConns)
	db.SetMaxIdleConns(options.MaxIdleConns)
	db.SetConnMaxLifetime(options.ConnMaxLifetime)
	db.SetConnMaxIdleTime(options.ConnMaxIdleTime)

	pingCtx, cancel := context.WithTimeout(ctx, options.PingTimeout)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		// The DSN carries a password, so it is never included in the error.
		return nil, fmt.Errorf("database: ping failed: %w", err)
	}
	return db, nil
}

// Migrate applies every pending migration from the given directory.
func Migrate(db *sql.DB, migrationsDir string) error {
	driver, err := migratepostgres.WithInstance(db, &migratepostgres.Config{})
	if err != nil {
		return fmt.Errorf("database: build migration driver: %w", err)
	}

	migrator, err := migrate.NewWithDatabaseInstance("file://"+migrationsDir, "postgres", driver)
	if err != nil {
		return fmt.Errorf("database: open migrations at %s: %w", migrationsDir, err)
	}

	if err := migrator.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("database: apply migrations: %w", err)
	}
	return nil
}
