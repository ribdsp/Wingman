package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/ribdsp/wingman/core/internal/config"
	"github.com/ribdsp/wingman/core/internal/database"
	"github.com/ribdsp/wingman/core/internal/repository"
	"github.com/ribdsp/wingman/core/internal/service"
)

// createUser makes an account from the command line.
//
// This exists because registration is closed by default, and something has to make the
// first account. It is the operator's own shell on the operator's own box, so it needs no
// credential — but it goes through the same service, the same validation and the same
// password hashing as every other path, because an account made by a second, simpler
// route is an account with a weaker password than the policy promises.
func createUser(args []string) error {
	flags := flag.NewFlagSet("createuser", flag.ContinueOnError)
	email := flags.String("email", "", "the account's email address")
	name := flags.String("name", "", "the display name shown in chats")
	flags.Usage = func() {
		// io.WriteString rather than a Fprint: the example below contains a printf verb,
		// and this text is not a format string.
		_, _ = io.WriteString(flags.Output(), `Usage: core createuser -email <address> -name <display name>

The password is read from standard input, and deliberately not taken as a flag: a
flag is in the process list and in the shell history. To type it without echo:

  read -rs CORE_PASSWORD && printf '%s' "$CORE_PASSWORD" | \
    core createuser -email you@example.com -name 'Your Name'

`)
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return err
	}

	password, err := readPassword(os.Stdin)
	if err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := newLogger(cfg)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := database.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer func() {
		if err := db.Close(); err != nil {
			log.Warn().Err(err).Msg("closing the database")
		}
	}()

	// Migrated here too, so the very first account can be made against an empty database
	// before the server has ever been started. Skipped when the operator has turned
	// automatic migration off, because then they are running them themselves.
	if cfg.AutoMigrate {
		if err := database.Migrate(db.DB, cfg.MigrationsDir); err != nil {
			return err
		}
	}

	users := repository.NewUserRepository(db)
	accounts, err := service.NewAccounts(service.AccountsDeps{
		Users:      users,
		Passwords:  users,
		Admin:      users,
		Sessions:   repository.NewSessionRepository(db),
		Revoker:    repository.NewSessionRepository(db),
		SessionTTL: cfg.Auth.SessionTTL,
		Logger:     log,
	})
	if err != nil {
		return err
	}

	// SystemActor: core acting on its own behalf. The service checks that the caller is
	// an operator, and this is the one caller that is one by construction — it is running
	// on the host, with the environment that holds the database password.
	user, err := accounts.Create(ctx, service.NewAccount{
		Email:       *email,
		DisplayName: *name,
		Password:    password,
	}, service.SystemActor())
	if err != nil {
		return err
	}

	// To stdout, so it can be captured. The id is what CORE_UNATTENDED_OWNER resolves to
	// at boot, and the address is what the person signs in with.
	fmt.Fprintf(os.Stdout, "created account %s for %s\n", user.ID, user.Email)
	return nil
}

// readPassword takes the password from standard input.
//
// One line, and everything on it after the newline is stripped is the password —
// including spaces, which are a legitimate part of a passphrase. Nothing here logs it,
// echoes it or puts it in an error message.
//
// Reading from stdin rather than a terminal in raw mode is deliberate: suppressing echo
// portably means another dependency for one prompt, while a pipe works the same in a
// shell, in a container and in a provisioning script. The usage text shows how to type
// one without it appearing on screen.
func readPassword(in io.Reader) (string, error) {
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read the password from standard input: %w", err)
	}

	password := strings.TrimRight(line, "\r\n")
	if password == "" {
		return "", errors.New("no password was given on standard input; see `core createuser -h`")
	}
	return password, nil
}
