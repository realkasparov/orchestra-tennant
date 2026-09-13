package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Фоновая служба: launchd на macOS, пользовательский unit systemd на Linux.
// Файл службы демон пишет сам — просить человека набрать его руками значит
// гарантировать ошибки в путях.

const (
	launchdLabel = "dev.orchestra.tennant"
	systemdUnit  = "orchestra-tennant.service"
)

func serviceKind() string {
	switch runtime.GOOS {
	case "darwin":
		return "launchd"
	case "linux":
		return "systemd --user"
	}
	return "не поддерживается на " + runtime.GOOS
}

func cmdService(args []string) error {
	fs, home := flagsFor("service")
	_ = fs.Parse(args)
	p := paths{*home}
	rest := fs.Args()
	if len(rest) != 1 {
		return errors.New("использование: orchestra-tennant service install | uninstall")
	}
	switch rest[0] {
	case "install":
		if _, err := readyConfig(p); err != nil {
			return err
		}
		if err := installService(p); err != nil {
			return err
		}
		fmt.Println("служба установлена и запущена")
		return nil
	case "uninstall":
		if err := uninstallService(p); err != nil {
			return err
		}
		fmt.Println("служба остановлена и удалена")
		return nil
	}
	return fmt.Errorf("неизвестное действие %q", rest[0])
}

// servicePath — где лежит файл службы.
func servicePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist"), nil
	case "linux":
		return filepath.Join(home, ".config", "systemd", "user", systemdUnit), nil
	}
	return "", fmt.Errorf("фоновая служба на %s не поддерживается — запускайте orchestra-tennant run", runtime.GOOS)
}

func installService(p paths) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return err
	}
	path, err := servicePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var body string
	switch runtime.GOOS {
	case "darwin":
		body = launchdPlist(exe, p)
	case "linux":
		body = systemdUnitFile(exe, p)
	}
	// Уже установленную службу сначала снимаем: bootstrap поверх живой
	// отвечает «already loaded», а старый plist мог указывать на другой файл.
	_ = stopService(path)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return err
	}
	return startService(path)
}

func uninstallService(p paths) error {
	path, err := servicePath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return errors.New("служба не установлена")
	}
	if err := stopService(path); err != nil {
		return err
	}
	return os.Remove(path)
}

func launchdPlist(exe string, p paths) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>` + launchdLabel + `</string>
  <key>ProgramArguments</key>
  <array>
    <string>` + xmlEscape(exe) + `</string>
    <string>run</string>
    <string>-home</string>
    <string>` + xmlEscape(p.home) + `</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ThrottleInterval</key><integer>5</integer>
  <key>WorkingDirectory</key><string>` + xmlEscape(p.home) + `</string>
  <key>StandardOutPath</key><string>` + xmlEscape(filepath.Join(p.home, "launchd.out.log")) + `</string>
  <key>StandardErrorPath</key><string>` + xmlEscape(filepath.Join(p.home, "launchd.err.log")) + `</string>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key><string>` + xmlEscape(servicePATH()) + `</string>
    <key>HOME</key><string>` + xmlEscape(userHome()) + `</string>
  </dict>
</dict>
</plist>
`
}

func systemdUnitFile(exe string, p paths) string {
	return `[Unit]
Description=orchestra-tennant — исполнитель тасок оркестратора
After=network.target

[Service]
ExecStart=` + exe + ` run -home ` + p.home + `
WorkingDirectory=` + p.home + `
Restart=always
RestartSec=5
Environment=PATH=` + servicePATH() + `

[Install]
WantedBy=default.target
`
}

// servicePATH — PATH для службы: у launchd и systemd он куцый, а claude
// обычно лежит в пользовательских папках. Берём текущий PATH человека — он
// настраивал демона из терминала, где claude находился.
func servicePATH() string {
	path := os.Getenv("PATH")
	for _, extra := range []string{"/opt/homebrew/bin", "/usr/local/bin", filepath.Join(userHome(), ".local", "bin")} {
		if !strings.Contains(":"+path+":", ":"+extra+":") {
			path += ":" + extra
		}
	}
	return path
}

func userHome() string {
	h, _ := os.UserHomeDir()
	return h
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

func launchdTarget() string { return "gui/" + strconv.Itoa(os.Getuid()) }

func startService(path string) error {
	switch runtime.GOOS {
	case "darwin":
		return run("launchctl", "bootstrap", launchdTarget(), path)
	case "linux":
		if err := run("systemctl", "--user", "daemon-reload"); err != nil {
			return err
		}
		return run("systemctl", "--user", "enable", "--now", systemdUnit)
	}
	return nil
}

func stopService(path string) error {
	switch runtime.GOOS {
	case "darwin":
		return run("launchctl", "bootout", launchdTarget()+"/"+launchdLabel)
	case "linux":
		return run("systemctl", "--user", "disable", "--now", systemdUnit)
	}
	return nil
}

// restartService перезапускает службу: после обновления бинаря процесс
// должен подняться новой версией.
func restartService() error {
	switch runtime.GOOS {
	case "darwin":
		return run("launchctl", "kickstart", "-k", launchdTarget()+"/"+launchdLabel)
	case "linux":
		return run("systemctl", "--user", "restart", systemdUnit)
	}
	return nil
}

// serviceState — установлена ли служба и идёт ли.
func serviceState() (installed bool, running bool, detail string) {
	path, err := servicePath()
	if err != nil {
		return false, false, err.Error()
	}
	if _, err := os.Stat(path); err != nil {
		return false, false, "не установлена"
	}
	switch runtime.GOOS {
	case "darwin":
		out, err := exec.Command("launchctl", "print", launchdTarget()+"/"+launchdLabel).Output()
		if err != nil {
			return true, false, "файл есть, но служба не загружена (launchctl print)"
		}
		for _, line := range strings.Split(string(out), "\n") {
			if strings.Contains(line, "state = ") {
				st := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "state ="))
				return true, st == "running", "launchd: " + st
			}
		}
		return true, true, "launchd: загружена"
	case "linux":
		out, _ := exec.Command("systemctl", "--user", "is-active", systemdUnit).Output()
		st := strings.TrimSpace(string(out))
		return true, st == "active", "systemd: " + st
	}
	return true, false, ""
}

func run(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
