package gws_test

import (
	"os"
	"testing"

	"github.com/tammersaleh/calendar-sync/internal/testhelpers"
)

// TestMain wires the test binary into the fake-gws harness. When the test
// binary is invoked indirectly through testhelpers.WithFakeGWS, the env
// sentinel is set and MaybeRunFakeGWS exits the process with the scenario
// response. Otherwise it returns and tests run normally.
func TestMain(m *testing.M) {
	testhelpers.MaybeRunFakeGWS()
	os.Exit(m.Run())
}
