package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
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

// cmdLogs показывает хвост журнала; -f следит за ним, переживая ротацию.
func cmdLogs(args []string) error {
	fs, home := flagsFor("logs")
	follow := fs.Bool("f", false, "следить за журналом")
	n := fs.Int("n", 50, "сколько последних строк показать")
	_ = fs.Parse(args)
	p := paths{*home}
	if err := printTail(os.Stdout, p.log(), *n); err != nil {
		return err
	}
	if !*follow {
		return nil
	}
	ctx, cancel := signalContext()
	defer cancel()
	return followLog(ctx, os.Stdout, p.log())
}

// printTail печатает последние n строк файла.
func printTail(w io.Writer, path string, n int) error {
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

// followLog дописывает новое из файла, пока не отменён ctx. Если файл
// подменили ротацией — открывает новый с начала.
func followLog(ctx context.Context, w io.Writer, path string) error {
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
				_, _ = w.Write(buf[:n])
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
