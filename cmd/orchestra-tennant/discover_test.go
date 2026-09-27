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

func TestGatedModels(t *testing.T) {
	// Версия ниже порога Opus 5.5, но выше порога Fable 5.1.
	if !cliAtLeast("2.1.272 (Claude Code)", gatedModels[0].MinCLI) || cliAtLeast("2.1.272 (Claude Code)", gatedModels[1].MinCLI) {
		t.Fatal("2.1.272: ожидалось fable51 да, opus55 нет")
	}
	if !cliAtLeast("2.1.280 (Claude Code)", gatedModels[1].MinCLI) {
		t.Fatal("2.1.280: ожидалось opus55 да")
	}
}
