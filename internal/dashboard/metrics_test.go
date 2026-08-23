package dashboard

import (
	"encoding/json"
	"expvar"
	"testing"
)

// TestExpvarFuncWrapper_RebindAndNil pins the rebinding contract that lets
// Run() be invoked more than once per process (tests do): a second
// publishExpvarFunc with the same name must swap the closure instead of
// panicking on duplicate registration, and scrapes must reflect the latest
// closure. A wrapper whose closure was never set reports JSON null so a
// /debug/vars scrape stays decodable.
func TestExpvarFuncWrapper_RebindAndNil(t *testing.T) {
	t.Parallel()

	publishExpvarFunc("test_expvar_wrapper_rebind", func() any { return "first" })
	w, ok := expvar.Get("test_expvar_wrapper_rebind").(*expvarFuncWrapper)
	if !ok {
		t.Fatal("publishExpvarFunc did not register the variable as an expvarFuncWrapper")
	}

	if got, want := w.String(), `"first"`; got != want {
		t.Fatalf("String() = %s, want %s", got, want)
	}

	// Rebind: must not panic and must replace the reported value.
	publishExpvarFunc("test_expvar_wrapper_rebind", func() any { return "second" })
	if got, want := w.String(), `"second"`; got != want {
		t.Fatalf("String() after rebind = %s, want %s", got, want)
	}
	if got := w.Value(); got != "second" {
		t.Fatalf("Value() after rebind = %v, want second", got)
	}

	// Nil-closure wrapper renders as null, not a panic or empty string.
	var nilWrapper expvarFuncWrapper
	if got, want := nilWrapper.String(), "null"; got != want {
		t.Fatalf("nil-closure String() = %q, want %q", got, want)
	}
	if got := nilWrapper.Value(); got != nil {
		t.Fatalf("nil-closure Value() = %v, want nil", got)
	}

	// The published expvar surface is the String() output; it must stay
	// valid JSON for every scrape consumer.
	var decoded any
	if err := json.Unmarshal([]byte(w.String()), &decoded); err != nil {
		t.Fatalf("String() is not valid JSON: %v", err)
	}
}
