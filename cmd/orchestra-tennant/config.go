package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Config — что демон знает о себе и об оркестраторе. Лежит в файле,
// доступном только владельцу: здесь постоянный ключ устройства.
type Config struct {
	// Orchestrator — адрес HTTP API оркестратора. Через него демон меняет
	// ключ подключения на постоянный и узнаёт, где сокет исполнителей.
	Orchestrator string `json:"orchestrator"`
	// Remote — адрес не на этой машине. Транспорта для такого случая пока
	// нет, и run об этом скажет, а не будет пытаться.
	Remote bool `json:"remote,omitempty"`

	DeviceKey  string `json:"device_key"`
	DeviceID   int64  `json:"device_id"`
	DeviceName string `json:"device_name"`
	// Socket — путь к сокету исполнителей, каким его назвал оркестратор.
	// Обновляется при каждом heartbeat: папка данных могла переехать.
	Socket string `json:"socket"`

	Models []string `json:"models"`
	Slots  int      `json:"slots"`
	// ProjectsDir — папка проектов машины, одна: проекты заводятся внутри.
	ProjectsDir string `json:"projects_dir"`

	// Configured — самопроверки пройдены. Без этого run отказывается:
	// настройка с проваленной проверкой не имеет права называться готовой.
	Configured   bool      `json:"configured"`
	ConfiguredAt time.Time `json:"configured_at,omitempty"`
}

var errNotConfigured = errors.New("демон не настроен — запустите: orchestra-tennant setup")

// paths — где что лежит в папке демона.
type paths struct{ home string }

func (p paths) config() string  { return filepath.Join(p.home, "config.json") }
func (p paths) log() string     { return filepath.Join(p.home, "agent.log") }
func (p paths) status() string  { return filepath.Join(p.home, "status.json") }
func (p paths) journal() string { return filepath.Join(p.home, "executor") }
func (p paths) tasks() string   { return filepath.Join(p.home, "tasks") }
func (p paths) skills() string  { return filepath.Join(p.home, "skills") }
func (p paths) mcpData() string { return p.home }
func (p paths) launchd() string { return filepath.Join(p.home, "launchd") }
func (p paths) mkdirAll() error { return os.MkdirAll(p.home, 0o700) }
func (p paths) exists() bool    { _, err := os.Stat(p.config()); return err == nil }
func (p paths) String() string  { return p.home }

// loadConfig читает конфигурацию; отсутствие файла — errNotConfigured.
func loadConfig(p paths) (*Config, error) {
	data, err := os.ReadFile(p.config())
	if errors.Is(err, os.ErrNotExist) {
		return nil, errNotConfigured
	}
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("конфигурация повреждена (%s): %w", p.config(), err)
	}
	return &c, nil
}

// saveConfig пишет конфигурацию атомарно и только для владельца.
func saveConfig(p paths, c *Config) error {
	if err := p.mkdirAll(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := p.config() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p.config())
}

// readyConfig — конфигурация, с которой можно работать.
func readyConfig(p paths) (*Config, error) {
	c, err := loadConfig(p)
	if err != nil {
		return nil, err
	}
	if !c.Configured {
		return nil, fmt.Errorf("настройка не завершена (самопроверки не пройдены) — запустите: orchestra-tennant setup")
	}
	if c.ProjectsDir == "" {
		return nil, fmt.Errorf("не задана папка проектов — запустите: orchestra-tennant setup")
	}
	if c.Remote {
		return nil, fmt.Errorf("оркестратор %s не на этой машине, а транспорта для удалённого пока нет — запустите setup с локальным адресом", c.Orchestrator)
	}
	return c, nil
}
