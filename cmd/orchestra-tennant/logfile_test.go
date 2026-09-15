package main

import "testing"

// Фильтр журнала: проект — только его таски, таска — только она; без
// фильтра проходит всё, строки демона без таски при фильтре отсеиваются.
func TestLogFilter(t *testing.T) {
	daemon := "2026/09/15 17:27:10 подключено к /tmp/agent.sock"
	snake3 := "2026/09/15 17:27:10 Project snake · Task 3 · Этап «Анализ задачи» — готово"
	snake4 := "2026/09/15 17:27:11 Project snake · Task 4 · Таска: в очереди"
	shop3 := "2026/09/15 17:27:12 Project my shop · Task 3 · Таска: готово"
	all := logFilter("", 0)
	for _, l := range []string{daemon, snake3, snake4, shop3} {
		if !all(l) {
			t.Errorf("без фильтра отсеяно: %q", l)
		}
	}
	proj := logFilter("Snake", 0)
	if !proj(snake3) || !proj(snake4) || proj(shop3) || proj(daemon) {
		t.Error("фильтр по проекту")
	}
	task := logFilter("snake", 3)
	if !task(snake3) || task(snake4) || task(shop3) {
		t.Error("фильтр по таске")
	}
	if p, id, ok := parseTaskLine(shop3); !ok || p != "my shop" || id != 3 {
		t.Errorf("разбор строки: %q %d %v", p, id, ok)
	}
}
