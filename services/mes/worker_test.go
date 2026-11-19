package main

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/aideveloper-cloud/kgarden-platform/internal/erpnext"
)

func TestRetryDelay(t *testing.T) {
	noMaterial := fmt.Errorf("insert stock entry: %w", &erpnext.Error{Status: 417, ExcType: "NegativeStockError"})
	other := errors.New("erpnext 500")

	// Waiting for raw material does not back off: every try is the same short interval.
	for _, attempts := range []int{1, 5, 9, 40} {
		if got := retryDelay(attempts, noMaterial, 10*time.Second); got != 10*time.Second {
			t.Errorf("material wait, attempt %d: got %s, want 10s", attempts, got)
		}
	}
	if got := retryDelay(7, noMaterial, 0); got != 10*time.Second {
		t.Errorf("material wait with no setting: got %s, want the 10s default", got)
	}

	// Other failures back off exponentially, capped at 5 minutes.
	for attempts, want := range map[int]time.Duration{1: 2 * time.Second, 3: 8 * time.Second, 8: 256 * time.Second, 9: 300 * time.Second} {
		if got := retryDelay(attempts, other, 10*time.Second); got != want {
			t.Errorf("failure, attempt %d: got %s, want %s", attempts, got, want)
		}
	}
}
