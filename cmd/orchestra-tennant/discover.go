package main

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// provider — один способ обращения к моделям, найденный на машине.
type provider struct {
	Key    string
	Title  string
	Detail string
	// Models — какие ключи моделей плана через него доступны. Пусто — способ
	// найден, но исполнитель им пока не пользуется: сказать об этом честнее,
	// чем молча выбросить.
	Models []string
}

// claudeModels — что умеет запускать исполнитель через claude CLI. Список
// один с agent.ModelID: ключ, которого там нет, свёлся бы к дефолту молча.
var claudeModels = []string{"fable", "opus", "sonnet", "haiku"}

// fable51MinCLI — с этой версии Claude Code принимает claude-fable-5-1;
// старая отвечает 400, поэтому машине со старым CLI ключ не предлагается.
var fable51MinCLI = [3]int{2, 1, 251}

// cliAtLeast — версия вида «2.1.272 (Claude Code)» не ниже min.
func cliAtLeast(version string, min [3]int) bool {
	fields := strings.Fields(version)
	if len(fields) == 0 {
		return false
	}
	parts := strings.Split(fields[0], ".")
	if len(parts) < 3 {
		return false
	}
	var v [3]int
	for i := range v {
		n, err := strconv.Atoi(parts[i])
		if err != nil {
			return false
		}
		v[i] = n
	}
	for i := range v {
		if v[i] != min[i] {
			return v[i] > min[i]
		}
	}
	return true
}

// discover ищет способы обращения к моделям: подписочные CLI, локальный
// ollama, ключи в окружении. Ничего не выбирается само — только предлагается.
func discover(ctx context.Context) []provider {
	var out []provider
	if path, err := exec.LookPath("claude"); err == nil {
		p := provider{Key: "claude", Title: "claude CLI", Detail: path, Models: claudeModels}
		if v := cliVersion(ctx, path); v != "" {
			p.Detail = v + " · " + path
			if cliAtLeast(v, fable51MinCLI) {
				p.Models = append(append([]string(nil), claudeModels...), "fable51")
			} else {
				p.Detail += " — Fable 5.1 недоступна: нужен Claude Code ≥ 2.1.251 (claude update)"
			}
		}
		out = append(out, p)
	}
	if path, err := exec.LookPath("codex"); err == nil {
		out = append(out, provider{Key: "codex", Title: "codex CLI",
			Detail: path + " — найден, но исполнитель пока запускает только claude"})
	}
	if ollamaAlive(ctx) {
		out = append(out, provider{Key: "ollama", Title: "ollama",
			Detail: "127.0.0.1:11434 — отвечает; используется индексом кода, не этапами"})
	} else if path, err := exec.LookPath("ollama"); err == nil {
		out = append(out, provider{Key: "ollama", Title: "ollama",
			Detail: path + " — установлен, но не запущен"})
	}
	for _, env := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY"} {
		if os.Getenv(env) != "" {
			out = append(out, provider{Key: strings.ToLower(env), Title: env,
				Detail: "задан в окружении (значение не показываем)"})
		}
	}
	return out
}

func cliVersion(ctx context.Context, path string) string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func ollamaAlive(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://127.0.0.1:11434/api/tags", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}

// usableModels — все ключи моделей, доступные через найденные способы.
func usableModels(ps []provider) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range ps {
		for _, m := range p.Models {
			if !seen[m] {
				seen[m] = true
				out = append(out, m)
			}
		}
	}
	return out
}
