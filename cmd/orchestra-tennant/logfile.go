package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Журнал демона — файл с ротацией по размеру. Ротация с первого дня: диск
// съедается не в день инцидента, а за месяцы тихой работы.

const (
	logMaxSize = 5 << 20 // байт в одном файле
	logKeep    = 3       // сколько старых файлов хранить: agent.log.1 … .3
)

// rotatingWriter пишет в файл и по достижении размера сдвигает старые копии.
type rotatingWriter struct {
	mu   sync.Mutex
	path string
	f    *os.File
	size int64
}

func openLog(path string) (*rotatingWriter, error) {
	w := &rotatingWriter{path: path}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *rotatingWriter) open() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	w.f, w.size = f, info.Size()
	return nil
}

func (w *rotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.size+int64(len(p)) > logMaxSize && w.size > 0 {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *rotatingWriter) rotate() error {
	w.f.Close()
	for i := logKeep - 1; i >= 1; i-- {
		_ = os.Rename(w.path+"."+strconv.Itoa(i), w.path+"."+strconv.Itoa(i+1))
	}
	if err := os.Rename(w.path, w.path+".1"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return w.open()
}

func (w *rotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.f.Close()
}

// cmdLog показывает хвост журнала; -f следит за ним, переживая ротацию.
// -project оставляет строки одного проекта, -task — одной его таски
// (таска без проекта не имеет смысла: номера тасок глобальны у
// оркестратора, но человек помнит их по проекту).
func cmdLog(args []string) error {
	fs, home := flagsFor("log")
	follow := fs.Bool("f", false, "следить за журналом")
	n := fs.Int("n", 50, "сколько последних строк показать")
	project := fs.String("project", "", "только строки этого проекта")
	task := fs.Int64("task", 0, "только строки этой таски (вместе с -project)")
	_ = fs.Parse(args)
	if *task != 0 && *project == "" {
		return errors.New("-task работает только вместе с -project")
	}
	p := paths{*home}
	filter := logFilter(*project, *task)
	if err := printTail(os.Stdout, p.log(), *n, filter); err != nil {
		return err
	}
	if !*follow {
		return nil
	}
	ctx, cancel := signalContext()
	defer cancel()
	return followLog(ctx, os.Stdout, p.log(), filter)
}

// logFilter — отбор строк журнала по проекту и таске. Без фильтра проходит
// всё; с фильтром — только строки тасок этого проекта (и этой таски).
func logFilter(project string, task int64) func(string) bool {
	if project == "" {
		return func(string) bool { return true }
	}
	want := strings.ToLower(strings.TrimSpace(project))
	return func(line string) bool {
		proj, id, ok := parseTaskLine(line)
		if !ok || strings.ToLower(proj) != want {
			return false
		}
		return task == 0 || id == task
	}
}

// parseTaskLine достаёт проект и номер таски из строки журнала вида
// «<дата> <время> Project <имя> · Task <N> · …».
func parseTaskLine(line string) (project string, task int64, ok bool) {
	i := strings.Index(line, "Project ")
	if i < 0 {
		return "", 0, false
	}
	rest := line[i+len("Project "):]
	j := strings.Index(rest, " · Task ")
	if j < 0 {
		return "", 0, false
	}
	project = rest[:j]
	num := rest[j+len(" · Task "):]
	if k := strings.Index(num, " · "); k >= 0 {
		num = num[:k]
	}
	id, err := strconv.ParseInt(strings.TrimSpace(num), 10, 64)
	if err != nil {
		return "", 0, false
	}
	return project, id, true
}

// printTail печатает последние n строк файла.
func printTail(w io.Writer, path string, n int, keep func(string) bool) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(w, "журнала ещё нет: %s\n", path)
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	ring := make([]string, 0, n)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for sc.Scan() {
		if !keep(sc.Text()) {
			continue
		}
		if len(ring) == n {
			ring = ring[1:]
		}
		ring = append(ring, sc.Text())
	}
	for _, line := range ring {
		fmt.Fprintln(w, line)
	}
	return nil
}

// followLog дописывает новое из файла построчно через фильтр, пока не
// отменён ctx. Если файл подменили ротацией — открывает новый с начала.
func followLog(ctx context.Context, w io.Writer, path string, keep func(string) bool) error {
	var tail string // незавершённая строка между чтениями
	emit := func(chunk []byte) {
		tail += string(chunk)
		for {
			i := strings.IndexByte(tail, '\n')
			if i < 0 {
				return
			}
			line := tail[:i]
			tail = tail[i+1:]
			if keep(line) {
				fmt.Fprintln(w, line)
			}
		}
	}
	var f *os.File
	var ino uint64
	var pos int64
	reopen := func() {
		if f != nil {
			f.Close()
			f = nil
		}
		nf, err := os.Open(path)
		if err != nil {
			return
		}
		f = nf
		ino = inode(nf)
		pos = 0
	}
	if nf, err := os.Open(path); err == nil {
		f, ino = nf, inode(nf)
		if info, err := nf.Stat(); err == nil {
			pos = info.Size()
		}
	}
	buf := make([]byte, 64*1024)
	for {
		select {
		case <-ctx.Done():
			if f != nil {
				f.Close()
			}
			return nil
		case <-time.After(300 * time.Millisecond):
		}
		if f == nil {
			reopen()
			if f == nil {
				continue
			}
		}
		for {
			n, err := f.ReadAt(buf, pos)
			if n > 0 {
				emit(buf[:n])
				pos += int64(n)
			}
			if err != nil {
				break
			}
		}
		if info, err := os.Stat(path); err != nil || inodeOf(info) != ino || info.Size() < pos {
			reopen()
		}
	}
}
