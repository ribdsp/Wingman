package config

import (
	"math"
	"os"
	"strconv"
	"strings"
	"time"
)

// Primitive environment readers.
//
// Each takes the fail callback rather than returning an error, because Load's
// contract is to report every problem at once. A reader that returned early would
// turn one bad value into one round of "fix it and try again" per field, which for a
// service an operator configures by hand over SSH is the difference between a
// five-minute job and an afternoon.
//
// Every reader returns its fallback on a parse failure. That keeps the rest of Load
// working against sane values so it can find the *next* problem instead of
// cascading; the recorded failure is what stops the boot.

func stringEnv(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func intEnv(key string, fallback int, fail func(string, ...any)) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		fail("%s must be an integer, got %q", key, raw)
		return fallback
	}
	return v
}

func int64Env(key string, fallback int64, fail func(string, ...any)) int64 {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		fail("%s must be an integer, got %q", key, raw)
		return fallback
	}
	return v
}

func floatEnv(key string, fallback float64, fail func(string, ...any)) float64 {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		fail("%s must be a number, got %q", key, raw)
		return fallback
	}
	// NaN compares false against every bound, so a NaN rate limit would pass a
	// "must be positive" check and then never throttle anything.
	if math.IsNaN(v) || math.IsInf(v, 0) {
		fail("%s must be a finite number, got %q", key, raw)
		return fallback
	}
	return v
}

func boolEnv(key string, fallback bool, fail func(string, ...any)) bool {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		// Refusing beats guessing. A typo read as false silently disables whatever
		// the operator was trying to switch on.
		fail("%s must be a boolean, got %q", key, raw)
		return fallback
	}
	return v
}

func durationEnv(key string, fallback time.Duration, fail func(string, ...any)) time.Duration {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	v, err := time.ParseDuration(raw)
	if err != nil {
		fail("%s must be a Go duration such as 1h or 30s, got %q", key, raw)
		return fallback
	}
	return v
}

func splitAndTrim(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
