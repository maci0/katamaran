package dashboard

import (
	"encoding/json"
	"expvar"
	"testing"
)

// TestPublishExpvarsRebindsToLatestApp pins the rebinding contract that lets
// Run() be invoked more than once per process (tests do): a second
// publishExpvars with a fresh App must not panic on duplicate registration,
// and scrapes must report the new App's counters as valid JSON.
func TestPublishExpvarsRebindsToLatestApp(t *testing.T) {
	first := &App{migrationsStarted: 1, migrationsFailed: 2}
	publishExpvars(first)

	if got, want := expvar.Get("migrations_started").String(), "1"; got != want {
		t.Fatalf("migrations_started after first publish = %s, want %s", got, want)
	}

	second := &App{migrationsStarted: 7, migrationsSucceeded: 3, migrationsFailed: 5}
	publishExpvars(second)

	for _, c := range []struct{ name, want string }{
		{"migrations_started", "7"},
		{"migrations_succeeded", "3"},
		{"migrations_failed", "5"},
	} {
		got := expvar.Get(c.name).String()
		if got != c.want {
			t.Errorf("%s after rebind = %s, want %s", c.name, got, c.want)
		}
		var decoded any
		if err := json.Unmarshal([]byte(got), &decoded); err != nil {
			t.Errorf("%s is not valid JSON: %v", c.name, err)
		}
	}
}
