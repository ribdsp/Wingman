package database

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"
)

// unreachableDSN points at a port nothing listens on, with a password in it. Every
// test using it asserts twice: that connecting fails, and that the failure does not
// carry the password.
const unreachableDSN = "postgres://wingman:hunter2-not-a-real-password@127.0.0.1:1/wingman?sslmode=disable"

func TestConnect_emptyDSN_refusesWithoutDialing(t *testing.T) {
	db, err := Connect(context.Background(), "")

	if err == nil {
		_ = db.Close()
		t.Fatal("expected an empty dsn to be refused")
	}
	if !strings.Contains(err.Error(), "dsn is empty") {
		t.Fatalf("error should say the dsn is empty, got %v", err)
	}
}

func TestConnect_unreachableServer_failsAtStartup(t *testing.T) {
	// A service that cannot reach its database has to fail here rather than on its
	// first decision, so this asserts Connect pings rather than only opening.
	db, err := Connect(context.Background(), unreachableDSN, WithPingTimeout(2*time.Second))

	if err == nil {
		_ = db.Close()
		t.Fatal("expected connecting to a closed port to fail")
	}
	if db != nil {
		t.Error("a failed Connect must not hand back a pool")
	}
	if !strings.Contains(err.Error(), "ping failed") {
		t.Fatalf("error should name the ping, got %v", err)
	}
}

func TestConnect_failure_neverEchoesThePassword(t *testing.T) {
	// The DSN is the one secret this package is handed. It appears in no error, at
	// any level of wrapping, because a startup failure is exactly what gets pasted
	// into an issue.
	_, err := Connect(context.Background(), unreachableDSN, WithPingTimeout(2*time.Second))
	if err == nil {
		t.Fatal("expected connecting to a closed port to fail")
	}

	if strings.Contains(err.Error(), "hunter2-not-a-real-password") {
		t.Fatalf("connect error carried the password: %v", err)
	}
	if strings.Contains(err.Error(), "wingman:") {
		t.Fatalf("connect error carried the userinfo section of the dsn: %v", err)
	}
}

func TestConnect_cancelledContext_doesNotHang(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := Connect(ctx, unreachableDSN)

	if err == nil {
		t.Fatal("expected a cancelled context to abort the connection attempt")
	}
}

func TestResolveOptions_noOverrides_usesDefaults(t *testing.T) {
	options := resolveOptions()

	if options.MaxOpenConns != defaultMaxOpenConns {
		t.Errorf("MaxOpenConns = %d, want %d", options.MaxOpenConns, defaultMaxOpenConns)
	}
	if options.MaxIdleConns != defaultMaxIdleConns {
		t.Errorf("MaxIdleConns = %d, want %d", options.MaxIdleConns, defaultMaxIdleConns)
	}
	if options.ConnMaxLifetime != defaultConnMaxLifetime {
		t.Errorf("ConnMaxLifetime = %s, want %s", options.ConnMaxLifetime, defaultConnMaxLifetime)
	}
	if options.ConnMaxIdleTime != defaultConnMaxIdleTime {
		t.Errorf("ConnMaxIdleTime = %s, want %s", options.ConnMaxIdleTime, defaultConnMaxIdleTime)
	}
	if options.PingTimeout != defaultPingTimeout {
		t.Errorf("PingTimeout = %s, want %s", options.PingTimeout, defaultPingTimeout)
	}
}

func TestWithMaxOpenConns_belowIdleDefault_pullsIdleDown(t *testing.T) {
	// An idle count above the open cap is a setting the pool cannot honour, so the
	// option resolves the conflict instead of leaving it to the driver.
	options := resolveOptions(WithMaxOpenConns(3))

	if options.MaxOpenConns != 3 {
		t.Errorf("MaxOpenConns = %d, want 3", options.MaxOpenConns)
	}
	if options.MaxIdleConns != 3 {
		t.Errorf("MaxIdleConns = %d, want it pulled down to 3", options.MaxIdleConns)
	}
}

func TestWithMaxOpenConns_aboveIdleDefault_leavesIdleAlone(t *testing.T) {
	options := resolveOptions(WithMaxOpenConns(50))

	if options.MaxOpenConns != 50 {
		t.Errorf("MaxOpenConns = %d, want 50", options.MaxOpenConns)
	}
	if options.MaxIdleConns != defaultMaxIdleConns {
		t.Errorf("MaxIdleConns = %d, want the default %d", options.MaxIdleConns, defaultMaxIdleConns)
	}
}

func TestOptions_nonsenseValues_keepTheDefaults(t *testing.T) {
	// Zero and negative are what an unset or fat-fingered environment variable
	// produces. A zero pool size means "unlimited" to database/sql and a zero ping
	// timeout means "already expired", so neither may be settable by accident.
	tests := map[string]struct {
		option Option
		check  func(*testing.T, Options)
	}{
		"zero open conns": {
			option: WithMaxOpenConns(0),
			check: func(t *testing.T, o Options) {
				if o.MaxOpenConns != defaultMaxOpenConns {
					t.Errorf("MaxOpenConns = %d, want %d", o.MaxOpenConns, defaultMaxOpenConns)
				}
			},
		},
		"negative open conns": {
			option: WithMaxOpenConns(-7),
			check: func(t *testing.T, o Options) {
				if o.MaxOpenConns != defaultMaxOpenConns {
					t.Errorf("MaxOpenConns = %d, want %d", o.MaxOpenConns, defaultMaxOpenConns)
				}
			},
		},
		"zero ping timeout": {
			option: WithPingTimeout(0),
			check: func(t *testing.T, o Options) {
				if o.PingTimeout != defaultPingTimeout {
					t.Errorf("PingTimeout = %s, want %s", o.PingTimeout, defaultPingTimeout)
				}
			},
		},
		"negative ping timeout": {
			option: WithPingTimeout(-time.Second),
			check: func(t *testing.T, o Options) {
				if o.PingTimeout != defaultPingTimeout {
					t.Errorf("PingTimeout = %s, want %s", o.PingTimeout, defaultPingTimeout)
				}
			},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			tc.check(t, resolveOptions(tc.option))
		})
	}
}

func TestWithPingTimeout_positive_isApplied(t *testing.T) {
	options := resolveOptions(WithPingTimeout(3 * time.Second))

	if options.PingTimeout != 3*time.Second {
		t.Errorf("PingTimeout = %s, want 3s", options.PingTimeout)
	}
}

func TestMigrate_unreachableDatabase_reportsTheDriver(t *testing.T) {
	// The migration driver interrogates the database before any file is read, so an
	// unreachable server fails here and not at "no such directory" — which is the
	// error an operator would otherwise go looking for in the wrong place.
	db, err := sql.Open("postgres", unreachableDSN)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	err = Migrate(db, "migrations")

	if err == nil {
		t.Fatal("expected migrating against an unreachable database to fail")
	}
	if !strings.Contains(err.Error(), "migration driver") {
		t.Fatalf("error should name the migration driver, got %v", err)
	}
	if strings.Contains(err.Error(), "hunter2-not-a-real-password") {
		t.Fatalf("migrate error carried the password: %v", err)
	}
}

// The success path of Migrate needs a live PostgreSQL, so it is an integration test
// rather than a unit one: the unit suite runs with no database and no network.
