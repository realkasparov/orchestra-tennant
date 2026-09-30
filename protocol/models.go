package protocol

import "regexp"

// Models — какие модели умеет запускать исполнитель: короткий ключ настроек,
// точный идентификатор сборки и название для человека. Таблица живёт только
// у исполнителя: оркестратор о моделях ничего не знает и получает каталог
// машины в hello (ModelCatalog).
var Models = []ModelInfo{
	{"fable", "claude-fable-5", "Fable 5"},
	{"fable51", "claude-fable-5-1", "Fable 5.1"}, // только через Claude Code ≥ 2.1.251
	{"opus55", "claude-opus-5-5", "Opus 5.5"},    // только через Claude Code ≥ 2.1.280
	{"opus", "claude-opus-5", "Opus 5"},
	{"sonnet", "claude-sonnet-5", "Sonnet 5"},           // средний уровень — ревью плана, ревью кода
	{"haiku", "claude-haiku-4-5-20251001", "Haiku 4.5"}, // дешёвые служебные вызовы (триаж сообщений, импорт)
}

// ModelInfo — одна модель каталога машины.
type ModelInfo struct {
	Key   string `json:"key"`
	ID    string `json:"id"`
	Title string `json:"title"`
}

// ModelID переводит ключ настроек в идентификатор сборки. Уже готовый
// идентификатор возвращается как есть: план несёт точную сборку, и
// переводить её второй раз нечего.
func ModelID(key string) string {
	if id, ok := ResolveModel(key); ok {
		return id
	}
	return Models[0].ID
}

// ResolveModel — идентификатор сборки по ключу или идентификатору; false —
// модели нет в таблице этого исполнителя.
func ResolveModel(s string) (string, bool) {
	for _, m := range Models {
		if m.Key == s || m.ID == s {
			return m.ID, true
		}
	}
	return "", false
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

// Catalog — каталог по списку ключей или идентификаторов, в порядке таблицы;
// неизвестные пропускаются.
func Catalog(models []string) []ModelInfo {
	want := map[string]bool{}
	for _, m := range models {
		if id, ok := ResolveModel(m); ok {
			want[id] = true
		}
	}
	var out []ModelInfo
	for _, m := range Models {
		if want[m.ID] {
			out = append(out, m)
		}
	}
	return out
}

var modelKeyRe = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,63}$`)

// ValidModelKey — ключ модели записан допустимо. Есть ли такая модель,
// решает машина: ключ, которого она не знает, просто не совпадёт с её
// каталогом.
func ValidModelKey(k string) bool { return modelKeyRe.MatchString(k) }

// ResolveModels переводит ключи моделей шагов плана в идентификаторы сборок
// этого исполнителя. Неизвестный ключ остаётся как есть — его назовёт
// проверка «на этой машине нет».
func (p *Plan) ResolveModels() {
	for i := range p.Steps {
		if id, ok := ResolveModel(p.Steps[i].Model); ok {
			p.Steps[i].Model = id
		}
	}
	for i := range p.Stages {
		if id, ok := ResolveModel(p.Stages[i].Model); ok {
			p.Stages[i].Model = id
		}
	}
	if p.QA != nil {
		if id, ok := ResolveModel(p.QA.Model); ok {
			p.QA.Model = id
		}
	}
}
