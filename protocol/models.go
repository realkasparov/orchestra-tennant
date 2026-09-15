package protocol

// Models — какую сборку означает короткий ключ из настроек. Таблица одна на
// оркестратор и исполнителей: оркестратор кладёт в план точный идентификатор,
// исполнитель объявляет в hello, какие идентификаторы умеет запускать.
var Models = []struct{ Key, ID string }{
	{"fable", "claude-fable-5"}, // Fable 5: claude-fable-5-1 требует Claude Code ≥ 2.1.251
	{"opus", "claude-opus-5"},
	{"sonnet", "claude-sonnet-5"},          // средний уровень — ревью плана, ревью кода
	{"haiku", "claude-haiku-4-5-20251001"}, // дешёвые служебные вызовы (триаж сообщений, импорт)
}

// ModelID переводит ключ настроек в идентификатор сборки. Уже готовый
// идентификатор возвращается как есть: план несёт точную сборку, и
// переводить её второй раз нечего.
func ModelID(key string) string {
	for _, m := range Models {
		if m.Key == key || m.ID == key {
			return m.ID
		}
	}
	return Models[0].ID
}

// ModelIDs — все сборки, которые умеет запускать этот исполнитель.
func ModelIDs() []string {
	out := make([]string, 0, len(Models))
	for _, m := range Models {
		out = append(out, m.ID)
	}
	return out
}

// ModelKey — ключ настроек по идентификатору сборки (для интерфейса).
func ModelKey(id string) string {
	for _, m := range Models {
		if m.ID == id {
			return m.Key
		}
	}
	return id
}
