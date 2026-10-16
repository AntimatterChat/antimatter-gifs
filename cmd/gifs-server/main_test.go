// Copyright (c) 2026-present Antimatter contributors.
// See LICENSE for license information.

package main

import "testing"

func TestVersionString(t *testing.T) {
	defer func(v, c string) { version, commit = v, c }(version, commit)

	version, commit = "dev", ""
	if got := versionString(); got != "gifs-server dev" {
		t.Errorf("versionString() = %q, want %q", got, "gifs-server dev")
	}
	version, commit = "1.2.0", "0123abcd"
	if got, want := versionString(), "gifs-server 1.2.0 (0123abcd)"; got != want {
		t.Errorf("versionString() = %q, want %q", got, want)
	}
}
