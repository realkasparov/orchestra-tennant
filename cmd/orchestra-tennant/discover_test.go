package main

import "testing"

func TestCliAtLeast(t *testing.T) {
	for v, want := range map[string]bool{
		"2.1.272 (Claude Code)": true, "2.1.251": true, "2.1.236 (Claude Code)": false,
		"2.2.0": true, "3.0.0": true, "1.9.999": false, "": false, "beta": false,
	} {
		if got := cliAtLeast(v, fable51MinCLI); got != want {
			t.Errorf("%q: %v, ожидалось %v", v, got, want)
		}
	}
}
