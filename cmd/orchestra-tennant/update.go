package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/realkasparov/orchestra-tennant/executor"
)

// Обновление из GitHub Releases: тот же источник, что у install.sh. Демон
// узнаёт последний релиз, сверяет контрольную сумму и подменяет собственный
// исполняемый файл; если стоит служба — перезапускает её.

const releasesRepo = "realkasparov/orchestra-tennant"

func cmdUpdate(args []string) error {
	fs, home := flagsFor("update")
	want := fs.String("version", "", "конкретный релиз (например, v0.3.1); по умолчанию последний")
	check := fs.Bool("check", false, "только проверить, есть ли новая версия")
	_ = fs.Parse(args)
	p := paths{*home}
	ctx, cancel := signalContext()
	defer cancel()

	version := *want
	if version == "" {
		var err error
		if version, err = latestRelease(ctx); err != nil {
			return err
		}
	}
	current := "v" + strings.TrimPrefix(executor.Version, "v")
	if version == current {
		fmt.Printf("orchestra-tennant %s — уже последняя версия\n", current)
		return nil
	}
	fmt.Printf("установлено %s, доступно %s\n", current, version)
	if *check {
		return nil
	}

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return err
	}
	fmt.Printf("скачиваю %s для %s/%s…\n", version, runtime.GOOS, runtime.GOARCH)
	tmp, err := downloadRelease(ctx, version, filepath.Dir(exe))
	if err != nil {
		return err
	}
	// Подмена атомарна: новый файл лежит рядом и переименовывается поверх
	// старого; работающий процесс продолжает жить на старом inode.
	if err := os.Rename(tmp, exe); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("замена %s: %w (нет прав на папку? установите в ~/.local/bin)", exe, err)
	}
	fmt.Printf("обновлено: %s → %s\n", exe, version)

	if installed, _, _ := serviceState(); installed {
		if err := restartService(); err != nil {
			fmt.Printf("служба не перезапустилась: %v — перезапустите вручную\n", err)
		} else {
			fmt.Println("служба перезапущена")
		}
	} else if st := readStatus(p); st != nil && processAlive(st.PID) {
		fmt.Printf("демон запущен вручную (pid %d) — остановите и запустите снова, чтобы он работал новой версией\n", st.PID)
	}
	return nil
}

// latestRelease — тег последнего релиза по редиректу GitHub: без API и токена.
func latestRelease(ctx context.Context) (string, error) {
	client := &http.Client{Timeout: 20 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://github.com/"+releasesRepo+"/releases/latest", nil)
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("GitHub недоступен: %w", err)
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	i := strings.LastIndex(loc, "/tag/")
	if resp.StatusCode/100 != 3 || i < 0 {
		return "", errors.New("не удалось узнать последний релиз (ответ GitHub без редиректа на тег)")
	}
	return loc[i+len("/tag/"):], nil
}

// downloadRelease скачивает архив релиза, сверяет sha256 из checksums.txt и
// распаковывает бинарь во временный файл в dir. Возвращает его путь.
func downloadRelease(ctx context.Context, version, dir string) (string, error) {
	name := fmt.Sprintf("orchestra-tennant_%s_%s", runtime.GOOS, runtime.GOARCH)
	base := "https://github.com/" + releasesRepo + "/releases/download/" + version + "/"
	sums, err := fetch(ctx, base+"checksums.txt")
	if err != nil {
		return "", fmt.Errorf("checksums.txt: %w", err)
	}
	var expected string
	for _, line := range strings.Split(string(sums), "\n") {
		if strings.HasSuffix(strings.TrimSpace(line), " "+name+".tar.gz") || strings.HasSuffix(strings.TrimSpace(line), "*"+name+".tar.gz") {
			expected = strings.Fields(line)[0]
		}
	}
	if expected == "" {
		return "", fmt.Errorf("в релизе %s нет сборки для %s/%s", version, runtime.GOOS, runtime.GOARCH)
	}
	archive, err := fetch(ctx, base+name+".tar.gz")
	if err != nil {
		return "", fmt.Errorf("архив: %w", err)
	}
	sum := sha256.Sum256(archive)
	if hex.EncodeToString(sum[:]) != expected {
		return "", errors.New("контрольная сумма не сошлась — архив повреждён или подменён")
	}
	gz, err := gzip.NewReader(strings.NewReader(string(archive)))
	if err != nil {
		return "", err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return "", errors.New("в архиве нет файла orchestra-tennant")
		}
		if err != nil {
			return "", err
		}
		if filepath.Base(h.Name) != "orchestra-tennant" || h.Typeflag != tar.TypeReg {
			continue
		}
		f, err := os.CreateTemp(dir, ".orchestra-tennant-update-*")
		if err != nil {
			return "", fmt.Errorf("временный файл рядом с бинарём: %w", err)
		}
		if _, err := io.Copy(f, tr); err != nil {
			f.Close()
			os.Remove(f.Name())
			return "", err
		}
		f.Close()
		if err := os.Chmod(f.Name(), 0o755); err != nil {
			os.Remove(f.Name())
			return "", err
		}
		return f.Name(), nil
	}
}

func fetch(ctx context.Context, url string) ([]byte, error) {
	client := &http.Client{Timeout: 5 * time.Minute}
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 200<<20))
}

// readStatus читает status.json живого процесса (nil, если его нет).
func readStatus(p paths) *runStatus {
	data, err := os.ReadFile(p.status())
	if err != nil {
		return nil
	}
	var st runStatus
	if json.Unmarshal(data, &st) != nil {
		return nil
	}
	return &st
}
