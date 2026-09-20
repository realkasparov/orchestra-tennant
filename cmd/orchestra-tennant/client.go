package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/realkasparov/orchestra-tennant/executor"
)

// client — HTTP-сторона общения с оркестратором: обмен ключа и heartbeat.
// Сами задания идут не здесь, а по сокету протоколом исполнителя.
type client struct {
	base string
	http *http.Client
}

func newClient(base string) *client {
	return &client{base: strings.TrimRight(base, "/"), http: &http.Client{Timeout: 15 * time.Second}}
}

// identity — как демон представляется: hostname, ОС, версия.
func identity() (host, osVer, version string) {
	host, _ = os.Hostname()
	return host, runtime.GOOS + "/" + runtime.GOARCH, executor.Version
}

// normalizeAddr приводит адрес к URL: «8765» и «localhost:8765» — тоже адреса.
func normalizeAddr(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errors.New("адрес пуст")
	}
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("адрес %q не разобрать", s)
	}
	u.Path, u.RawQuery, u.Fragment = strings.TrimRight(u.Path, "/"), "", ""
	return u.String(), nil
}

// isLocalAddr — адрес на этой машине: только такой демон умеет обслуживать.
// Кроме localhost и loopback-адресов это любое имя вида *.localhost (RFC
// 6761: такие имена всегда означают эту машину — так оркестратор живёт за
// локальным reverse-proxy) и любое имя, которое резолвится только в loopback
// (алиас в /etc/hosts).
func isLocalAddr(base string) bool {
	u, err := url.Parse(base)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	ips, err := net.LookupIP(host)
	if err != nil || len(ips) == 0 {
		return false
	}
	for _, ip := range ips {
		if !ip.IsLoopback() {
			return false
		}
	}
	return true
}

// reachable — оркестратор отвечает по адресу. Открытый маршрут без сессии.
func (c *client) reachable(ctx context.Context) error {
	req, _ := http.NewRequestWithContext(ctx, "GET", c.base+"/api/auth/state", nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("оркестратор ответил %s", resp.Status)
	}
	var st struct {
		Bootstrap *bool `json:"bootstrap"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil || st.Bootstrap == nil {
		return errors.New("по адресу отвечает не оркестратор")
	}
	return nil
}

type paired struct {
	DeviceKey string `json:"device_key"`
	Device    struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	} `json:"device"`
}

// pair меняет ключ подключения на постоянный ключ устройства.
func (c *client) pair(ctx context.Context, pairKey string) (*paired, error) {
	host, osVer, version := identity()
	var out paired
	status, err := c.post(ctx, "/api/devices/pair", "", map[string]any{
		"pair_key": pairKey, "hostname": host, "os": osVer, "agent_version": version,
	}, &out)
	if err != nil {
		return nil, err
	}
	if status == 401 {
		return nil, errors.New("ключ подключения не принят: просрочен, уже использован или отозван — выпустите новый на карточке устройства")
	}
	if status != 200 {
		return nil, fmt.Errorf("оркестратор ответил %d", status)
	}
	return &out, nil
}

type heartbeat struct {
	DeviceID  int64  `json:"device_id"`
	Socket    string `json:"socket"`
	SchemaMin int    `json:"schema_min"`
	SchemaMax int    `json:"schema_max"`
}

var errKeyRejected = errors.New("ключ устройства не принят: устройство отозвано или ключ перевыпущен — пройдите setup заново")

// heartbeat предъявляет постоянный ключ и узнаёт, где сокет и какие версии
// плана понимает оркестратор.
func (c *client) heartbeat(ctx context.Context, deviceKey string) (*heartbeat, error) {
	host, osVer, version := identity()
	var out heartbeat
	status, err := c.post(ctx, "/api/devices/heartbeat", deviceKey, map[string]any{
		"hostname": host, "os": osVer, "agent_version": version,
	}, &out)
	if err != nil {
		return nil, err
	}
	if status == 401 {
		return nil, errKeyRejected
	}
	if status != 200 {
		return nil, fmt.Errorf("оркестратор ответил %d", status)
	}
	return &out, nil
}

func (c *client) post(ctx context.Context, path, deviceKey string, body any, out any) (int, error) {
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", c.base+path, bytes.NewReader(raw))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if deviceKey != "" {
		req.Header.Set("X-Device-Key", deviceKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == 200 && out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.StatusCode, fmt.Errorf("ответ оркестратора не разобрать: %w", err)
		}
	}
	return resp.StatusCode, nil
}
