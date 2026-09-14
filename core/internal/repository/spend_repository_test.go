package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	// The timezone database is embedded into the test binary. Two of the tests below
	// assert behaviour that only differs outside UTC — a half-hour offset and a
	// daylight-saving change — and on a machine with no system zoneinfo they would
	// otherwise silently fall back to UTC and pass while proving nothing.
	_ "time/tzdata"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

func spendRowSet() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"user_id", "run_id", "provider", "model", "tokens_in", "tokens_out", "occurred_at",
	})
}

func mustLoad(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("load %s: %v", name, err)
	}
	return loc
}

func TestSpendRepository_record_filesTheChargeAgainstTheAccount(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewSpendRepository(db, time.UTC)
	mock.ExpectExec(`INSERT INTO token_spend`).
		WithArgs("usr_01", "run_01", "anthropic", "claude-opus-5", int64(1_200), int64(340), fixedNow).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Act
	err := repo.Record(context.Background(), Spend{
		UserID: "usr_01", RunID: "run_01",
		Provider: "anthropic", Model: "claude-opus-5",
		TokensIn: 1_200, TokensOut: 340, OccurredAt: fixedNow,
	})

	// Assert
	if err != nil {
		t.Fatalf("record spend: %v", err)
	}
}

func TestSpendRepository_record_aChargeWithNoRunIsStoredAsNull(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewSpendRepository(db, time.UTC)
	mock.ExpectExec(`INSERT INTO token_spend`).
		WithArgs("usr_01", nil, "openai", "gpt-5", int64(80), int64(20), fixedNow).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Act
	err := repo.Record(context.Background(), Spend{
		UserID: "usr_01", Provider: "openai", Model: "gpt-5",
		TokensIn: 80, TokensOut: 20, OccurredAt: fixedNow,
	})

	// Assert
	// Not every model call belongs to a run — a title generation does not — and an
	// empty string would be a foreign key to a run that does not exist.
	if err != nil {
		t.Fatalf("record spend: %v", err)
	}
}

func TestSpendRepository_record_refusesANegativeOrTimestamplessCharge(t *testing.T) {
	// Arrange
	db, _ := newMock(t)
	repo := NewSpendRepository(db, time.UTC)

	// Act
	negative := repo.Record(context.Background(), Spend{
		UserID: "usr_01", TokensOut: -500, OccurredAt: fixedNow,
	})
	timeless := repo.Record(context.Background(), Spend{
		UserID: "usr_01", TokensIn: 100,
	})

	// Assert
	// A negative row would refund an allowance nobody granted, which is a way to spend
	// past a daily cap without the cap ever reading as exceeded. A charge with no
	// timestamp belongs to no day, so it would be invisible to every daily total.
	if negative == nil {
		t.Error("a negative charge was accepted")
	}
	if timeless == nil {
		t.Error("a charge with no timestamp was accepted")
	}
}

func TestSpend_total_countsBothDirections(t *testing.T) {
	// Arrange
	spend := Spend{TokensIn: 1_200, TokensOut: 340}

	// Act
	got := spend.Total()

	// Assert
	// A budget that counted only output would undercharge every long prompt, which is
	// the exact shape of call an agent loop makes: the history is re-sent each time.
	if got != 1_540 {
		t.Errorf("Total() = %d; want 1540", got)
	}
}

// Ledger is what the continuation ladder is handed before each iteration, so this test
// pins the half-open day window as parameters rather than as a clause the query hides.
func TestSpendRepository_ledger_readsOnlyTheAccountsSpendForToday(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewSpendRepository(db, time.UTC)
	start, end := repo.DayBounds(fixedNow)
	mock.ExpectQuery(`sum\(tokens_total\).*FROM token_spend\s+WHERE user_id = \$1 AND occurred_at >= \$2 AND occurred_at < \$3`).
		WithArgs("usr_01", start, end).
		WillReturnRows(sqlmock.NewRows([]string{"sum"}).AddRow(int64(48_000)))

	// Act
	ledger, err := repo.Ledger(context.Background(), "usr_01", fixedNow)

	// Assert
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	if !ledger.Readable {
		t.Error("a successful read came back unreadable")
	}
	if ledger.TokensToday != 48_000 {
		t.Errorf("tokensToday = %d; want 48000", ledger.TokensToday)
	}
}

// This is the fail-closed one. Reporting a broken read as zero spend would be
// indistinguishable from an untouched budget, which is the single confusion the ladder
// exists to prevent — so the failure returns an unreadable ledger *and* the error, and a
// caller that ignores either still stops.
func TestSpendRepository_ledger_failureIsUnreadableRatherThanZero(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewSpendRepository(db, time.UTC)
	mock.ExpectQuery(`FROM token_spend`).
		WillReturnError(errors.New("connection reset"))

	// Act
	ledger, err := repo.Ledger(context.Background(), "usr_01", fixedNow)

	// Assert
	if err == nil {
		t.Fatal("a failed read was reported as a successful one")
	}
	if ledger.Readable {
		t.Error("a failed read came back readable")
	}
	// Zero here would read to the ladder as a completely unspent budget.
	if ledger.TokensToday != 0 {
		t.Errorf("tokensToday = %d alongside the error; want 0", ledger.TokensToday)
	}
}

func TestSpendRepository_ledger_anAccountThatHasSpentNothingReadsAsZeroAndReadable(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewSpendRepository(db, time.UTC)
	mock.ExpectQuery(`FROM token_spend`).
		WillReturnRows(sqlmock.NewRows([]string{"sum"}).AddRow(int64(0)))

	// Act
	ledger, err := repo.Ledger(context.Background(), "usr_new", fixedNow)

	// Assert
	// coalesce is what makes this distinguishable from the failure above: no rows sums
	// to NULL in SQL, and a NULL scanned into an int64 would be an error rather than a
	// new account with a full allowance.
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	if !ledger.Readable || ledger.TokensToday != 0 {
		t.Errorf("ledger = %+v; want readable and zero", ledger)
	}
}

// The total is a running figure rather than an increment, so re-sending one after a failed
// push cannot inflate it. That is what lets the goal engine's sampler be at-least-once
// without any bookkeeping on this side.
func TestSpendRepository_totalForDay_isARunningTotalForTheWholeInstance(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewSpendRepository(db, time.UTC)
	start, end := repo.DayBounds(fixedNow)
	mock.ExpectQuery(`sum\(tokens_total\).*FROM token_spend\s+WHERE occurred_at >= \$1 AND occurred_at < \$2`).
		WithArgs(start, end).
		WillReturnRows(sqlmock.NewRows([]string{"sum"}).AddRow(int64(310_500)))

	// Act
	total, err := repo.TotalForDay(context.Background(), fixedNow)

	// Assert
	if err != nil {
		t.Fatalf("read total for day: %v", err)
	}
	if total != 310_500 {
		t.Errorf("total = %d; want 310500", total)
	}
}

func TestSpendRepository_totalsByUserForDay_ordersTheBiggestSpenderFirst(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewSpendRepository(db, time.UTC)
	start, end := repo.DayBounds(fixedNow)
	mock.ExpectQuery(`GROUP BY user_id\s+ORDER BY tokens DESC, user_id ASC\s+LIMIT \$3`).
		WithArgs(start, end, maxPageLimit).
		WillReturnRows(sqlmock.NewRows([]string{"user_id", "tokens"}).
			AddRow("usr_02", int64(280_000)).
			AddRow("usr_01", int64(30_500)))

	// Act
	totals, err := repo.TotalsByUserForDay(context.Background(), fixedNow, 100_000)

	// Assert
	// An operator watching one number climb cannot tell a busy team from one runaway
	// loop. Biggest first is what answers that in the first row.
	if err != nil {
		t.Fatalf("read totals by user: %v", err)
	}
	if len(totals) != 2 {
		t.Fatalf("got %d rows; want 2", len(totals))
	}
	if totals[0].UserID != "usr_02" || totals[0].Tokens != 280_000 {
		t.Errorf("first row = %+v; want the largest spender", totals[0])
	}
}

func TestSpendRepository_forRun_makesAnExpensiveRunAnswerable(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewSpendRepository(db, time.UTC)
	mock.ExpectQuery(`FROM token_spend\s+WHERE run_id = \$1\s+ORDER BY occurred_at ASC`).
		WithArgs("run_01").
		WillReturnRows(spendRowSet().
			AddRow("usr_01", "run_01", "anthropic", "claude-opus-5", 900, 120, fixedNow).
			AddRow("usr_01", "run_01", "anthropic", "claude-opus-5", 1_800, 260, fixedNow.Add(time.Second)))

	// Act
	spends, err := repo.ForRun(context.Background(), "run_01")

	// Assert
	if err != nil {
		t.Fatalf("read spend for run: %v", err)
	}
	if len(spends) != 2 {
		t.Fatalf("got %d charges; want 2", len(spends))
	}
	// Which model actually answered is recorded per charge, so a run that silently fell
	// back to a cheaper model is visible in the bill rather than only in the total.
	if spends[1].Total() != 2_060 || spends[1].Model != "claude-opus-5" {
		t.Errorf("second charge = %+v; want 2060 tokens on claude-opus-5", spends[1])
	}
}

func TestSpendRepository_dayBounds_resetsAtLocalMidnightNotUTCMidnight(t *testing.T) {
	// Arrange
	bangkok := mustLoad(t, "Asia/Bangkok") // UTC+07:00, no DST
	repo := NewSpendRepository(nil, bangkok)
	// 2026-09-11 20:00 at UTC+07:00 is still 13:00 UTC on the same date, but 02:00
	// local is the previous UTC day — which is where a UTC boundary goes wrong.
	now := time.Date(2026, 9, 11, 2, 0, 0, 0, bangkok)

	// Act
	start, end := repo.DayBounds(now)

	// Assert
	// A cap that reset at UTC midnight would, seven hours ahead of UTC, reset at 7am
	// local — in the middle of their working morning, halfway through the day it was
	// meant to bound.
	if start.Day() != 11 || start.Hour() != 0 || start.Location() != bangkok {
		t.Errorf("start = %s; want local midnight on the 11th", start)
	}
	if end.Day() != 12 || end.Hour() != 0 {
		t.Errorf("end = %s; want local midnight on the 12th", end)
	}
	if !start.Before(now) || !end.After(now) {
		t.Errorf("now %s is outside [%s, %s)", now, start, end)
	}
}

func TestSpendRepository_dayBounds_landsOnMidnightInAHalfHourOffsetZone(t *testing.T) {
	// Arrange
	kolkata := mustLoad(t, "Asia/Kolkata") // UTC+05:30
	repo := NewSpendRepository(nil, kolkata)
	now := time.Date(2026, 9, 11, 9, 15, 0, 0, kolkata)

	// Act
	start, _ := repo.DayBounds(now)

	// Assert
	// Truncate works in UTC, so truncating to 24h here would land on 05:30 local — no
	// midnight at all. Building the instant from the calendar date is what avoids that.
	if start.Hour() != 0 || start.Minute() != 0 {
		t.Errorf("start = %s; want 00:00 local", start)
	}
}

// A fixed 24 hours would either clip an hour of spend or count an hour twice on the two
// days a year that are not 24 hours long. AddDate is what makes the window the local day.
func TestSpendRepository_dayBounds_spansTheShortDayAcrossADaylightSavingChange(t *testing.T) {
	// Arrange
	newYork := mustLoad(t, "America/New_York")
	repo := NewSpendRepository(nil, newYork)
	// 2026-03-08 is the spring-forward date in the US: 02:00 becomes 03:00, so the
	// local day is 23 hours long.
	springForward := time.Date(2026, 3, 8, 12, 0, 0, 0, newYork)

	// Act
	start, end := repo.DayBounds(springForward)

	// Assert
	if got := end.Sub(start); got != 23*time.Hour {
		t.Errorf("the short day spans %s; want 23h", got)
	}
	if end.Day() != 9 || end.Hour() != 0 {
		t.Errorf("end = %s; want local midnight on the 9th", end)
	}
}

func TestSpendRepository_dayBounds_spansTheLongDayAcrossADaylightSavingChange(t *testing.T) {
	// Arrange
	newYork := mustLoad(t, "America/New_York")
	repo := NewSpendRepository(nil, newYork)
	// 2026-11-01 is the fall-back date: 02:00 becomes 01:00, so the local day is 25
	// hours long and a fixed 24 would leave an hour of spend in neither day.
	fallBack := time.Date(2026, 11, 1, 12, 0, 0, 0, newYork)

	// Act
	start, end := repo.DayBounds(fallBack)

	// Assert
	if got := end.Sub(start); got != 25*time.Hour {
		t.Errorf("the long day spans %s; want 25h", got)
	}
}

func TestSpendRepository_dayBounds_isHalfOpenSoAMidnightChargeBelongsToOneDay(t *testing.T) {
	// Arrange
	repo := NewSpendRepository(nil, time.UTC)
	midnight := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)

	// Act
	start, end := repo.DayBounds(midnight)
	_, previousEnd := repo.DayBounds(midnight.Add(-time.Nanosecond))

	// Assert
	// occurred_at >= start AND occurred_at < end. A charge landing exactly on midnight
	// has to be counted once, and an inclusive upper bound would count it twice — once
	// against a cap that was about to reset and once against the one that just did.
	if !start.Equal(midnight) {
		t.Errorf("start = %s; want the midnight instant itself", start)
	}
	if !previousEnd.Equal(midnight) {
		t.Errorf("the previous day ended at %s; want it to end where this one starts", previousEnd)
	}
	if !end.After(start) {
		t.Errorf("the window [%s, %s) is empty", start, end)
	}
}

func TestNewSpendRepository_nilLocationIsUTCRatherThanAFailureToStart(t *testing.T) {
	// Arrange & Act
	repo := NewSpendRepository(nil, nil)
	start, _ := repo.DayBounds(fixedNow)

	// Assert
	// A ledger that refused to construct would stop the service starting. A daily
	// boundary in the wrong zone is a reporting inconvenience; the per-run cap, which is
	// the one that stops a runaway loop, does not depend on it at all.
	if start.Location() != time.UTC {
		t.Errorf("location = %s; want UTC", start.Location())
	}
}
