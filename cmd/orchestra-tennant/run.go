package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	tennant "github.com/realkasparov/orchestra-tennant"
	"github.com/realkasparov/orchestra-tennant/agent"
	"github.com/realkasparov/orchestra-tennant/codeindex"
	"github.com/realkasparov/orchestra-tennant/executor"
	"github.com/realkasparov/orchestra-tennant/protocol"
)

// status.json — что демон сообщает о себе другим командам (status) и
// человеку: жив ли, на связи ли, сколько заданий ведёт.
type runStatus struct {
	PID       int       `json:"pid"`
	Version   string    `json:"version"`
	Connected bool      `json:"connected"`
	Since     time.Time `json:"since"`
	Jobs      int       `json:"jobs"`
	UpdatedAt time.Time `json:"updated_at"`
	LastError string    `json:"last_error,omitempty"`
}

// cmdRun — работа демона: подключение к сокету оркестратора и исполнение
// заданий до сигнала остановки. Без завершённой настройки не запускается:
// угадывать параметры — значит молча делать не то.
func cmdRun(args []string) error {
	fs, home := flagsFor("run")
	_ = fs.Parse(args)
	p := paths{*home}
	cfg, err := readyConfig(p)
	if err != nil {
		return err
	}
	if err := p.mkdirAll(); err != nil {
		return err
	}
	logw, err := openLog(p.log())
	if err != nil {
		return err
	}
	defer logw.Close()
	var out io.Writer = logw
	if isTerminal(os.Stdout) {
		out = io.MultiWriter(os.Stdout, logw)
	}
	logger := log.New(out, "", log.LstdFlags)
	logf := logger.Printf
	logf("orchestra-tennant %s запускается: устройство «%s», оркестратор %s, мест %d, модели %v, проекты в %s",
		executor.Version, cfg.DeviceName, cfg.Orchestrator, cfg.Slots, cfg.Models, cfg.ProjectsDir)

	// Скиллы — из бинаря: демон на чужой машине не зависит от репозитория.
	skillsDir, err := executor.MaterializeSkills(tennant.Skills, p.home)
	if err != nil {
		return err
	}
	userHome, _ := os.UserHomeDir()
	if err := executor.InstallSkills(skillsDir, userHome); err != nil {
		logf("скиллы: %v", err)
	}

	index := codeindex.NewManager(p.home)
	index.Log = func(projectID int64, text string) { logf("codeindex[%d]: %s", projectID, text) }
	host, osVer, version := identity()
	ex, err := executor.New(executor.Config{
		DeviceKey: cfg.DeviceKey, Hostname: host, OS: osVer, Version: version,
		Slots: cfg.Slots, Models: modelIDs(cfg.Models), Skills: executor.StageSkills, ProjectsDir: cfg.ProjectsDir,
		JournalDir: p.journal(), Log: logf, Trace: true,
	}, &executor.Pipeline{DataDir: p.home, ProjectsDir: cfg.ProjectsDir, Index: index, EnsureIndex: true, Log: logf})
	if err != nil {
		return err
	}

	ctx, cancel := signalContext()
	defer cancel()
	st := &statusWriter{path: p.status(), st: runStatus{PID: os.Getpid(), Version: version, Since: time.Now()}}
	st.write()
	defer os.Remove(p.status())
	go st.watch(ctx, ex)

	c := newClient(cfg.Orchestrator)
	dial := func() (protocol.Conn, error) {
		// Сокет узнаём у оркестратора каждый раз: его папка данных могла
		// переехать, а ключ — быть отозван, и об этом лучше узнать здесь, а
		// не по молчанию сокета.
		hctx, hcancel := context.WithTimeout(ctx, 15*time.Second)
		hb, err := c.heartbeat(hctx, cfg.DeviceKey)
		hcancel()
		if err != nil {
			st.setError(err.Error())
			if errors.Is(err, errKeyRejected) {
				logf("%v", err)
			}
			return nil, err
		}
		if hb.Socket != "" && hb.Socket != cfg.Socket {
			cfg.Socket = hb.Socket
			_ = saveConfig(p, cfg)
		}
		conn, err := protocol.Dial(cfg.Socket)
		if err != nil {
			st.setError(err.Error())
			return nil, fmt.Errorf("сокет %s: %w", cfg.Socket, err)
		}
		st.setError("")
		logf("подключено к %s", cfg.Socket)
		return conn, nil
	}
	err = ex.Serve(ctx, dial)
	logf("orchestra-tennant остановлен: %v", err)
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// modelIDs переводит ключи из конфигурации в идентификаторы сборок: в hello
// идут точные сборки, и план приходит с ними же.
func modelIDs(keys []string) []string {
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, agent.ModelID(k))
	}
	return out
}

// statusWriter держит status.json свежим.
type statusWriter struct {
	path string
	st   runStatus
}

func (s *statusWriter) setError(msg string) { s.st.LastError = msg }

func (s *statusWriter) watch(ctx context.Context, ex *executor.Executor) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.st.Connected = ex.Connected()
			s.st.Jobs = ex.ActiveJobs()
			s.write()
		}
	}
}

func (s *statusWriter) write() {
	s.st.UpdatedAt = time.Now()
	data, _ := json.MarshalIndent(s.st, "", "  ")
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err == nil {
		_ = os.Rename(tmp, s.path)
	}
}
