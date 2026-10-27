package main

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	// All existing tests, including subprocesses, opt out of the real user DB.
	if err := os.Setenv("ATHENA_BRIDGE_USAGE_DB", "off"); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}
