package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"
)

// cmdStatus — настроен ли демон, идёт ли служба, на связи ли процесс.
func cmdStatus(args []string) error {
	fs, home := flagsFor("status")
	_ = fs.Parse(args)
	p := paths{*home}
	fmt.Printf("orchestra-tennant %s · папка %s\n", versionString(), p)

	cfg, err := loadConfig(p)
	switch {
	case errors.Is(err, errNotConfigured):
		fmt.Println("настройка:   не выполнена — orchestra-tennant setup")
		return nil
	case err != nil:
		return err
	case cfg.Remote:
		fmt.Printf("настройка:   оркестратор %s не на этой машине; транспорта для него нет\n", cfg.Orchestrator)
	case !cfg.Configured:
		fmt.Println("настройка:   не завершена (самопроверки не пройдены) — orchestra-tennant setup")
	default:
		fmt.Printf("настройка:   готова %s\n", cfg.ConfiguredAt.Format("2006-01-02 15:04"))
	}
	fmt.Printf("оркестратор: %s\n", cfg.Orchestrator)
	fmt.Printf("устройство:  «%s» (#%d)\n", cfg.DeviceName, cfg.DeviceID)
	fmt.Printf("модели:      %s · мест: %d\n", strings.Join(cfg.Models, ", "), cfg.Slots)
	fmt.Printf("проекты:     %s\n", cfg.ProjectsDir)

	_, _, detail := serviceState()
	fmt.Printf("служба:      %s\n", detail)

	// Связь проверяется здесь и сейчас, а не по записи демона: иначе не
	// понять, лежит демон или оркестратор.
	fmt.Printf("оркестратор: %s\n", probeOrchestrator(cfg))

	data, err := os.ReadFile(p.status())
	if err != nil {
		fmt.Println("демон:       не запущен")
		return nil
	}
	var st runStatus
	if err := json.Unmarshal(data, &st); err != nil || !processAlive(st.PID) {
		fmt.Println("демон:       не запущен (осталась запись прошлого запуска)")
		return nil
	}
	conn := "нет связи с оркестратором"
	if st.Connected {
		conn = "на связи"
	}
	fmt.Printf("демон:       pid %d, %s, заданий: %d, с %s\n", st.PID, conn, st.Jobs, st.Since.Format("2006-01-02 15:04"))
	if st.LastError != "" {
		fmt.Printf("последняя ошибка: %s\n", st.LastError)
	}
	if time.Since(st.UpdatedAt) > 30*time.Second {
		fmt.Printf("внимание: состояние не обновлялось %s\n", time.Since(st.UpdatedAt).Round(time.Second))
	}
	return nil
}

// probeOrchestrator — живая проверка: оркестратор отвечает, ключ принят,
// сокет на месте.
func probeOrchestrator(cfg *Config) string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := newClient(cfg.Orchestrator)
	if err := c.reachable(ctx); err != nil {
		return "не отвечает — " + err.Error()
	}
	hb, err := c.heartbeat(ctx, cfg.DeviceKey)
	if err != nil {
		return "отвечает, но " + err.Error()
	}
	if _, err := os.Stat(hb.Socket); err != nil {
		return "отвечает, ключ принят, но сокета " + hb.Socket + " нет"
	}
	return "отвечает, ключ принят, сокет " + hb.Socket
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}
