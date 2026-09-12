package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/realkasparov/orchestra-tennant/agent"
	"github.com/realkasparov/orchestra-tennant/protocol"
)

// check — одна самопроверка с явным итогом.
type check struct {
	Name   string
	Detail string
	Err    error
}

// runChecks — четыре проверки в конце настройки: оркестратор найден, ключ
// принят, модели отвечают, версия схемы согласована. Каждая печатается своей
// строкой; провал любой означает, что настройка не завершена. Попутно
// heartbeat обновляет в конфигурации сокет и номер устройства.
func runChecks(ctx context.Context, cfg *Config, w io.Writer, modelCheck func(context.Context, string) error) bool {
	c := newClient(cfg.Orchestrator)
	var hb *heartbeat
	checks := []check{{Name: "Оркестратор найден"}, {Name: "Ключ принят"}, {Name: "Модели отвечают"}, {Name: "Версия схемы согласована"}}

	if err := c.reachable(ctx); err != nil {
		checks[0].Err = fmt.Errorf("%s: %v — оркестратор запущен и адрес верный?", cfg.Orchestrator, err)
	} else {
		checks[0].Detail = cfg.Orchestrator
	}

	if checks[0].Err == nil {
		var err error
		hb, err = c.heartbeat(ctx, cfg.DeviceKey)
		switch {
		case err != nil:
			checks[1].Err = err
		case hb.Socket == "":
			checks[1].Err = errors.New("оркестратор не назвал сокет исполнителей — он старше демона; обновите оркестратор")
		default:
			cfg.DeviceID, cfg.Socket = hb.DeviceID, hb.Socket
			if _, err := os.Stat(hb.Socket); err != nil {
				checks[1].Err = fmt.Errorf("ключ принят, но сокет исполнителей %s не найден — оркестратор на этой машине?", hb.Socket)
			} else {
				checks[1].Detail = fmt.Sprintf("устройство «%s», сокет %s", cfg.DeviceName, hb.Socket)
			}
		}
	} else {
		checks[1].Err = errors.New("не проверялось: оркестратор недоступен")
	}

	if len(cfg.Models) == 0 {
		checks[2].Err = errors.New("не выбрано ни одной модели")
	} else {
		var failed []string
		for _, m := range cfg.Models {
			if err := modelCheck(ctx, m); err != nil {
				failed = append(failed, m+": "+firstLine(err.Error()))
			}
		}
		if len(failed) > 0 {
			checks[2].Err = errors.New(strings.Join(failed, "; ") + " — доставьте claude CLI и войдите в подписку (claude login)")
		} else {
			checks[2].Detail = strings.Join(cfg.Models, ", ")
		}
	}

	switch {
	case hb == nil:
		checks[3].Err = errors.New("не проверялось: ключ не принят")
	case hb.SchemaMin > protocol.SchemaVersion:
		checks[3].Err = fmt.Errorf("оркестратор требует схему плана не ниже %d, демон понимает до %d — обновите демона", hb.SchemaMin, protocol.SchemaVersion)
	case hb.SchemaMax < protocol.MinSchemaVersion:
		checks[3].Err = fmt.Errorf("оркестратор умеет схему до %d, демон требует не ниже %d — обновите оркестратор", hb.SchemaMax, protocol.MinSchemaVersion)
	default:
		v := protocol.SchemaVersion
		if hb.SchemaMax < v {
			v = hb.SchemaMax
		}
		checks[3].Detail = fmt.Sprintf("схема %d", v)
	}

	ok := true
	for _, ch := range checks {
		if ch.Err != nil {
			ok = false
			fmt.Fprintf(w, "  ✗ %s — %v\n", ch.Name, ch.Err)
		} else {
			fmt.Fprintf(w, "  ✓ %s — %s\n", ch.Name, ch.Detail)
		}
	}
	return ok
}

// checkModel спрашивает у модели одно слово: единственный способ узнать, что
// она отвечает, — спросить. Стоит это меньше цента и делается один раз.
func checkModel(ctx context.Context, key string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	dir, err := os.MkdirTemp("", "orchestra-tennant-check-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	res, err := agent.Run(ctx, agent.RunOpts{
		Prompt: "Ответь одним словом: OK", Model: agent.ModelID(key), Effort: "low",
		CWD: dir, AllowedTools: []string{"Read"},
	}, func(agent.StreamEvent) {})
	if err != nil {
		return err
	}
	if res == nil || strings.TrimSpace(res.FullText) == "" {
		return errors.New("пустой ответ")
	}
	return nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
