package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/realkasparov/orchestra-tennant/protocol"
)

// Конфигурация с ключом лежит в файле только для владельца.
func TestConfigSavedForOwnerOnly(t *testing.T) {
	p := paths{filepath.Join(t.TempDir(), "home")}
	cfg := &Config{Orchestrator: "http://127.0.0.1:1", DeviceKey: "secret", Slots: 2}
	if err := saveConfig(p, cfg); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p.config())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("права конфигурации %o, ожидалось 600", info.Mode().Perm())
	}
	if dir, _ := os.Stat(p.home); dir.Mode().Perm() != 0o700 {
		t.Errorf("права папки %o, ожидалось 700", dir.Mode().Perm())
	}
	back, err := loadConfig(p)
	if err != nil || back.DeviceKey != "secret" || back.Slots != 2 {
		t.Fatalf("прочитано %+v %v", back, err)
	}
}

// run без настройки отправляет в setup, а не угадывает параметры; настройка
// без пройденных проверок — тоже.
func TestRunRefusesWithoutSetup(t *testing.T) {
	p := paths{t.TempDir()}
	if _, err := readyConfig(p); !errors.Is(err, errNotConfigured) {
		t.Fatalf("без конфигурации: %v", err)
	}
	_ = saveConfig(p, &Config{Orchestrator: "http://127.0.0.1:1", DeviceKey: "k", Configured: false})
	if _, err := readyConfig(p); err == nil || !strings.Contains(err.Error(), "setup") {
		t.Fatalf("с проваленными проверками: %v", err)
	}
	_ = saveConfig(p, &Config{Orchestrator: "http://10.0.0.5:8765", Remote: true, DeviceKey: "k", Configured: true, ProjectsDir: "/p"})
	if _, err := readyConfig(p); err == nil || !strings.Contains(err.Error(), "не на этой машине") {
		t.Fatalf("удалённый оркестратор: %v", err)
	}
	// Конфигурация старого демона без папки проектов — тоже в setup.
	_ = saveConfig(p, &Config{Orchestrator: "http://127.0.0.1:1", DeviceKey: "k", Configured: true})
	if _, err := readyConfig(p); err == nil || !strings.Contains(err.Error(), "папка проектов") {
		t.Fatalf("без папки проектов: %v", err)
	}
}

// Журнал ротируется по размеру и хранит ограниченное число старых файлов.
func TestLogRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.log")
	w, err := openLog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	line := bytes.Repeat([]byte("x"), 1024)
	line[len(line)-1] = '\n'
	for i := 0; i < (logMaxSize/len(line))*(logKeep+3); i++ {
		if _, err := w.Write(line); err != nil {
			t.Fatal(err)
		}
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != logKeep+1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("файлов журнала %d, ожидалось %d: %v", len(entries), logKeep+1, names)
	}
	if info, _ := os.Stat(path); info.Size() > logMaxSize {
		t.Errorf("текущий файл %d байт больше потолка", info.Size())
	}
	var out bytes.Buffer
	if err := printTail(&out, path, 3, func(string) bool { return true }); err != nil || strings.Count(out.String(), "\n") != 3 {
		t.Errorf("хвост: %v %q", err, out.String())
	}
}

// fakeOrchestrator — HTTP-сторона оркестратора для проверок настройки.
type fakeOrchestrator struct {
	socket    string
	schemaMin int
	schemaMax int
	goodKey   string
	pairKey   string
	paired    int
}

func (f *fakeOrchestrator) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/auth/state", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"bootstrap": false, "allow_registration": true, "user": nil})
	})
	mux.HandleFunc("POST /api/devices/pair", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			PairKey string `json:"pair_key"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in.PairKey != f.pairKey || f.paired > 0 {
			w.WriteHeader(401)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "ключ подключения недействителен"})
			return
		}
		f.paired++
		_ = json.NewEncoder(w).Encode(map[string]any{"device_key": f.goodKey, "device": map[string]any{"id": 7, "name": "Ноутбук"}})
	})
	mux.HandleFunc("POST /api/devices/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Device-Key") != f.goodKey {
			w.WriteHeader(401)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "ключ недействителен"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"device_id": 7, "live_window_sec": 90,
			"socket": f.socket, "schema_min": f.schemaMin, "schema_max": f.schemaMax})
	})
	return mux
}

func newFake(t *testing.T) (*fakeOrchestrator, *httptest.Server) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "agent.sock")
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f := &fakeOrchestrator{socket: sock, schemaMin: protocol.MinSchemaVersion, schemaMax: protocol.SchemaVersion,
		goodKey: "device-key-1", pairKey: "pair-1"}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	return f, srv
}

func okModel(context.Context, string) error { return nil }

// Все четыре проверки проходят; heartbeat заполняет сокет и номер устройства.
func TestChecksPass(t *testing.T) {
	f, srv := newFake(t)
	cfg := &Config{Orchestrator: srv.URL, DeviceKey: f.goodKey, DeviceName: "Ноутбук", Models: []string{"fable"}}
	var out bytes.Buffer
	if !runChecks(context.Background(), cfg, &out, okModel) {
		t.Fatalf("проверки провалены:\n%s", out.String())
	}
	if cfg.Socket != f.socket || cfg.DeviceID != 7 {
		t.Errorf("сокет/устройство не заполнены: %+v", cfg)
	}
	if strings.Count(out.String(), "✓") != 4 {
		t.Errorf("ожидалось четыре галочки:\n%s", out.String())
	}
}

// Ключ отозван: проверка называет причину, а зависящая от неё — что не
// проверялась.
func TestChecksKeyRejected(t *testing.T) {
	_, srv := newFake(t)
	cfg := &Config{Orchestrator: srv.URL, DeviceKey: "stale", Models: []string{"fable"}}
	var out bytes.Buffer
	if runChecks(context.Background(), cfg, &out, okModel) {
		t.Fatal("проверки прошли с отозванным ключом")
	}
	s := out.String()
	if !strings.Contains(s, "✗ Ключ принят — ключ устройства не принят") || !strings.Contains(s, "✗ Версия схемы согласована — не проверялось") {
		t.Errorf("вывод:\n%s", s)
	}
}

// Оркестратор требует схему новее: проверка говорит обновить демона.
func TestChecksSchemaMismatch(t *testing.T) {
	f, srv := newFake(t)
	f.schemaMin, f.schemaMax = protocol.SchemaVersion+1, protocol.SchemaVersion+1
	cfg := &Config{Orchestrator: srv.URL, DeviceKey: f.goodKey, Models: []string{"fable"}}
	var out bytes.Buffer
	if runChecks(context.Background(), cfg, &out, okModel) {
		t.Fatal("проверки прошли при расхождении схемы")
	}
	if !strings.Contains(out.String(), "обновите демона") {
		t.Errorf("вывод:\n%s", out.String())
	}
}

// Модель не отвечает: провал с причиной, остальные проверки — по-прежнему
// свои.
func TestChecksModelUnavailable(t *testing.T) {
	f, srv := newFake(t)
	cfg := &Config{Orchestrator: srv.URL, DeviceKey: f.goodKey, Models: []string{"fable", "opus"}}
	bad := func(_ context.Context, m string) error {
		if m == "opus" {
			return errors.New("claude exited: not logged in")
		}
		return nil
	}
	var out bytes.Buffer
	if runChecks(context.Background(), cfg, &out, bad) {
		t.Fatal("проверки прошли с недоступной моделью")
	}
	s := out.String()
	if !strings.Contains(s, "✗ Модели отвечают — opus: claude exited: not logged in") || strings.Count(s, "✓") != 3 {
		t.Errorf("вывод:\n%s", s)
	}
}

// Оркестратор не отвечает: первая проверка называет адрес, вторая и четвёртая
// не притворяются пройденными.
func TestChecksUnreachable(t *testing.T) {
	cfg := &Config{Orchestrator: "http://127.0.0.1:1", DeviceKey: "k", Models: []string{"fable"}}
	var out bytes.Buffer
	if runChecks(context.Background(), cfg, &out, okModel) {
		t.Fatal("проверки прошли без оркестратора")
	}
	if strings.Count(out.String(), "✗") != 3 {
		t.Errorf("вывод:\n%s", out.String())
	}
}

// Пайринг: чужой и повторно использованный ключ отвергаются с объяснением.
func TestPair(t *testing.T) {
	f, srv := newFake(t)
	c := newClient(srv.URL)
	ctx := context.Background()
	if _, err := c.pair(ctx, "wrong"); err == nil || !strings.Contains(err.Error(), "выпустите новый") {
		t.Fatalf("чужой ключ: %v", err)
	}
	pd, err := c.pair(ctx, f.pairKey)
	if err != nil || pd.DeviceKey != f.goodKey || pd.Device.Name != "Ноутбук" {
		t.Fatalf("пайринг: %+v %v", pd, err)
	}
	if _, err := c.pair(ctx, f.pairKey); err == nil {
		t.Fatal("ключ принят второй раз")
	}
}

// Адреса: порт, host:port и URL приводятся к одному виду; локальность
// определяется по хосту.
func TestNormalizeAddr(t *testing.T) {
	cases := map[string]string{
		"127.0.0.1:8765":           "http://127.0.0.1:8765",
		"http://localhost:8765/":   "http://localhost:8765",
		"https://orch.example.com": "https://orch.example.com",
	}
	for in, want := range cases {
		got, err := normalizeAddr(in)
		if err != nil || got != want {
			t.Errorf("%q → %q %v, ожидалось %q", in, got, err, want)
		}
	}
	if _, err := normalizeAddr(""); err == nil {
		t.Error("пустой адрес принят")
	}
	if !isLocalAddr("http://127.0.0.1:8765") || !isLocalAddr("http://localhost:1") || isLocalAddr("http://10.0.0.5:8765") {
		t.Error("локальность адреса определена неверно")
	}
	// Оркестратор за локальным reverse-proxy: имя *.localhost — эта машина.
	if !isLocalAddr("http://agent-service.localhost") || !isLocalAddr("http://orchestra.localhost:8080") {
		t.Error("*.localhost должен считаться этой машиной")
	}
	if isLocalAddr("http://example.com") {
		t.Error("внешнее имя принято за локальное")
	}
}

// Модели из флага проверяются против найденного; прежний выбор
// ограничивается тем, что есть сейчас.
func TestParseModels(t *testing.T) {
	usable := []string{"fable", "opus", "sonnet"}
	if got, err := parseModels("fable, haiku", usable); err == nil {
		t.Errorf("недоступная модель принята: %v", got)
	}
	if got, err := parseModels("sonnet,fable", usable); err != nil || len(got) != 2 {
		t.Errorf("флаг: %v %v", got, err)
	}
	if got := intersect([]string{"opus", "haiku"}, usable); strings.Join(got, ",") != "opus" {
		t.Errorf("прежний выбор: %v", got)
	}
}

// Без терминала вопрос без значения по умолчанию — ошибка, а не зависание;
// список без прежнего выбора — все модели.
func TestSilentAsker(t *testing.T) {
	pr := silent{}
	if _, err := pr.text("Ключ", "", nil); err == nil {
		t.Error("без терминала вопрос без умолчания должен падать")
	}
	if v, err := pr.text("Адрес", "http://x", nil); err != nil || v != "http://x" {
		t.Errorf("умолчание без терминала: %q %v", v, err)
	}
	if got, _ := pr.multi("Модели", []string{"a", "b"}, nil); len(got) != 2 {
		t.Errorf("по умолчанию все: %v", got)
	}
	if got, _ := pr.multi("Модели", []string{"a", "b"}, []string{"b"}); strings.Join(got, ",") != "b" {
		t.Errorf("прежний выбор: %v", got)
	}
}

// Подсказки пути — существующие папки по началу имени, в форме ввода.
func TestDirSuggestions(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"orchestra-projects", "orchid", "other", ".hidden"} {
		if err := os.Mkdir(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "orc.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	got := dirSuggestions(filepath.Join(root, "orc"))
	want := []string{filepath.Join(root, "orchestra-projects") + "/", filepath.Join(root, "orchid") + "/"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("подсказки: %v, ожидалось %v", got, want)
	}
	if got := dirSuggestions(root + "/"); len(got) != 3 {
		t.Errorf("все папки без скрытых: %v", got)
	}
	if got := dirSuggestions(""); got != nil {
		t.Errorf("пустой ввод: %v", got)
	}
	// Одна тильда раньше роняла подсказку: имя домашней папки длиннее ввода.
	if got := dirSuggestions("~"); len(got) != 1 || got[0] != "~/" {
		t.Errorf("тильда: %v", got)
	}
}

// Файл службы указывает на этот бинарь и папку демона, а не на что-то
// подставленное.
func TestServiceFiles(t *testing.T) {
	p := paths{"/Users/me/.orchestra-tennant"}
	plist := launchdPlist("/opt/bin/orchestra-tennant", p)
	for _, want := range []string{"<string>/opt/bin/orchestra-tennant</string>", "<string>run</string>", "<string>/Users/me/.orchestra-tennant</string>", "<key>KeepAlive</key><true/>", "<key>RunAtLoad</key><true/>"} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist без %q", want)
		}
	}
	unit := systemdUnitFile("/opt/bin/orchestra-tennant", p)
	for _, want := range []string{`ExecStart="/opt/bin/orchestra-tennant" run -home "/Users/me/.orchestra-tennant"`, "WorkingDirectory=/Users/me/.orchestra-tennant", "Restart=always", "WantedBy=default.target"} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit без %q", want)
		}
	}
}
