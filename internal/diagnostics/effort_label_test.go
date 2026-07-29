package diagnostics

import "testing"

func TestEffortLabelsTreatUndefinedAndNullAsUnavailable(t *testing.T) {
	for _, value := range []string{"", " undefined ", "NULL"} {
		if got := emptyLabel(value); got != "unavailable" {
			t.Fatalf("emptyLabel(%q) = %q, want unavailable", value, got)
		}
	}
	if got := emptyLabel(" Extra_High "); got != "extra_high" {
		t.Fatalf("emptyLabel preserves normalized effort = %q, want extra_high", got)
	}
}
