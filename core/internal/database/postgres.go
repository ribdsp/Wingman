// Package database opens and migrates Wingman core's PostgreSQL database.
//
// Core has its own database, separate from the goal engine's. That separation is
// deliberate: this one holds users' conversations and grows with traffic, while the
// goal engine's holds the audit log and the spending policy. Losing one should not
// mean losing the other, and a migration here should not be able to lock a table the
// spending gate reads.
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

// Pool defaults. Larger than the goal engine's, because core serves interactive
// traffic as well as background run workers, and a worker holds a connection while it
// writes each step of a transcript.
const (
	defaultMaxOpenConns    = 25
	defaultMaxIdleConns    = 10
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

// WithMaxOpenConns caps the pool size. Idle is pulled down with it, because an idle
// count above the open cap is a configuration that cannot be honoured.
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

// resolveOptions applies the defaults and then the caller's overrides. It is split
// out so the pool settings can be asserted without a server to connect to.
func resolveOptions(opts ...Option) Options {
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
	return options
}

// Connect opens a pool and verifies it can reach the server. A service that cannot
// reach its database should fail at startup, not four tool calls into somebody's run.
func Connect(ctx context.Context, dsn string, opts ...Option) (*sqlx.DB, error) {
	if dsn == "" {
		return nil, errors.New("database: dsn is empty")
	}

	options := resolveOptions(opts...)

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
		// The DSN carries a password, so it is never included in the error. The
		// driver's own message names a host and port and nothing more.
		return nil, fmt.Errorf("database: ping failed: %w", err)
	}
	return db, nil
}

// Migrate applies every pending migration from the given directory.
//
// Self-hosting is the deployment model, so this runs at startup by default. It is
// still the operator's switch (AUTO_MIGRATE), because on a multi-instance deployment
// exactly one process should be the one that migrates.
func Migrate(db *sql.DB, migrationsDir string) error {
	driver, err := migratepostgres.WithInstance(db, &migratepostgres.Config{})
	if err != nil {
		return fmt.Errorf("database: build migration driver: %w", err)
	}

	migrator, err := migrate.NewWithDatabaseInstance("file://"+migrationsDir, "postgres", driver)
	if err != nil {
		return fmt.Errorf("database: open migrations at %s: %w", migrationsDir, err)
	}

	// ErrNoChange is the normal case on every restart after the first.
	if err := migrator.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("database: apply migrations: %w", err)
	}
	return nil
}
