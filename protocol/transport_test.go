package protocol

import (
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Один набор проверок на обе реализации: встроенный исполнитель ходит через
// канал, демон — через сокет, и разъехаться они не должны. Сокет иначе доехал
// бы до этапа 3 неопробованным.
func eachTransport(t *testing.T, run func(t *testing.T, a, b Conn)) {
	t.Helper()
	t.Run("внутри процесса", func(t *testing.T) {
		a, b := Pipe()
		defer a.Close()
		defer b.Close()
		run(t, a, b)
	})
	t.Run("через сокет", func(t *testing.T) {
		a, b, stop := socketPair(t)
		defer stop()
		run(t, a, b)
	})
}

// shortSocket даёт путь заведомо короче предела ядра: временные папки тестов
// на macOS сами по себе длинные, а имена подтестов удлиняют их ещё.
func shortSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "sock")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "a.sock")
}

func socketPair(t *testing.T) (client, server Conn, stop func()) {
	t.Helper()
	path := shortSocket(t)
	ln, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	type accepted struct {
		c   net.Conn
		err error
	}
	ch := make(chan accepted, 1)
	go func() {
		c, aerr := ln.Accept()
		ch <- accepted{c, aerr}
	}()
	cl, err := Dial(path)
	if err != nil {
		ln.Close()
		t.Fatal(err)
	}
	got := <-ch
	if got.err != nil {
		t.Fatal(got.err)
	}
	srv := NewConn(got.c)
	return cl, srv, func() {
		cl.Close()
		srv.Close()
		ln.Close()
	}
}

func TestTransportDeliversInOrder(t *testing.T) {
	eachTransport(t, func(t *testing.T, a, b Conn) {
		for i := 0; i < 20; i++ {
			body, _ := json.Marshal(map[string]int{"n": i})
			if err := a.Send(&Envelope{Type: MsgEvent, JobID: "j1", Body: body}); err != nil {
				t.Fatalf("отправка %d: %v", i, err)
			}
		}
		for i := 0; i < 20; i++ {
			e, err := b.Recv()
			if err != nil {
				t.Fatalf("приём %d: %v", i, err)
			}
			var m map[string]int
			if err := json.Unmarshal(e.Body, &m); err != nil {
				t.Fatal(err)
			}
			if m["n"] != i {
				t.Fatalf("порядок нарушен: пришло %d вместо %d", m["n"], i)
			}
			if e.Type != MsgEvent || e.JobID != "j1" {
				t.Fatalf("конверт искажён: %+v", e)
			}
		}
	})
}

// План целиком проходит по проводу неизменным: это основной груз протокола, и
// на нём же ловится ограничение на размер кадра, если оно где-то заведётся.
func TestTransportCarriesWholePlan(t *testing.T) {
	eachTransport(t, func(t *testing.T, a, b Conn) {
		p := validPlan()
		// Условие задачи бывает длинным: сотня килобайт для описания задачи с
		// приложенными логами — обычное дело.
		p.Prompt = strings.Repeat("подробное условие задачи. ", 6000)
		raw, err := p.Encode()
		if err != nil {
			t.Fatal(err)
		}
		if len(raw) < 128*1024 {
			t.Fatalf("проверка слишком мелкая: %d байт", len(raw))
		}
		// Приём идёт параллельно отправке: запись в сокет блокирующая, буфер
		// ядра меньше плана, и последовательное «отправить, потом прочитать»
		// встало бы навсегда — ровно так же, как встал бы оркестратор,
		// пишущий исполнителю, который не читает.
		type got struct {
			e   *Envelope
			err error
		}
		ch := make(chan got, 1)
		go func() {
			e, rerr := b.Recv()
			ch <- got{e, rerr}
		}()
		if err := a.Send(&Envelope{Type: MsgOffer, JobID: "j1", Body: raw}); err != nil {
			t.Fatal(err)
		}
		var e *Envelope
		select {
		case g := <-ch:
			if g.err != nil {
				t.Fatal(g.err)
			}
			e = g.e
		case <-time.After(10 * time.Second):
			t.Fatal("план не дошёл")
		}
		back, err := Decode(e.Body)
		if err != nil {
			t.Fatalf("план не пережил передачу: %v", err)
		}
		if back.Prompt != p.Prompt || back.TaskID != p.TaskID {
			t.Error("план изменился при передаче")
		}
	})
}

// Отправка идёт из нескольких горутин: события этапа и продление лиза живут
// в разных потоках, и записи не должны перемешиваться внутри одного значения.
func TestTransportSendIsConcurrencySafe(t *testing.T) {
	eachTransport(t, func(t *testing.T, a, b Conn) {
		const n = 50
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				body, _ := json.Marshal(map[string]int{"n": i})
				if err := a.Send(&Envelope{Type: MsgEvent, Body: body}); err != nil {
					t.Errorf("отправка: %v", err)
				}
			}(i)
		}
		seen := map[int]bool{}
		done := make(chan struct{})
		go func() {
			defer close(done)
			for i := 0; i < n; i++ {
				e, err := b.Recv()
				if err != nil {
					t.Errorf("приём: %v", err)
					return
				}
				var m map[string]int
				if err := json.Unmarshal(e.Body, &m); err != nil {
					t.Errorf("кадр повреждён: %v", err)
					return
				}
				seen[m["n"]] = true
			}
		}()
		wg.Wait()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("приём не завершился")
		}
		if len(seen) != n {
			t.Errorf("дошло %d сообщений из %d", len(seen), n)
		}
	})
}

// Закрытие с одной стороны должно быть видно другой, а не оставлять её ждать
// вечно: иначе оркестратор держал бы задание за исполнителем, которого нет.
func TestTransportCloseIsVisible(t *testing.T) {
	eachTransport(t, func(t *testing.T, a, b Conn) {
		a.Close()
		errCh := make(chan error, 1)
		go func() {
			_, err := b.Recv()
			errCh <- err
		}()
		select {
		case err := <-errCh:
			if err == nil {
				t.Error("приём после закрытия не дал ошибки")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("приём завис на закрытом соединении")
		}
		if err := a.Send(&Envelope{Type: MsgEvent}); err == nil {
			t.Error("отправка в закрытое соединение прошла")
		}
	})
}

// Уже отправленное не теряется при закрытии: отправитель считает его
// доставленным, и молча выбросить его нельзя.
func TestPipeDrainsBeforeEOF(t *testing.T) {
	a, b := Pipe()
	body, _ := json.Marshal(map[string]string{"k": "v"})
	if err := a.Send(&Envelope{Type: MsgEvent, Body: body}); err != nil {
		t.Fatal(err)
	}
	a.Close()
	e, err := b.Recv()
	if err != nil {
		t.Fatalf("отправленное до закрытия потерялось: %v", err)
	}
	if e.Type != MsgEvent {
		t.Errorf("пришло не то: %+v", e)
	}
	if _, err := b.Recv(); err != io.EOF {
		t.Errorf("после дочитывания ожидался EOF, получено: %v", err)
	}
}

// Сокет доступен только владельцу: локально это и есть аутентификация, и
// зонтик umask не должен оставлять его открытым другим пользователям машины.
func TestSocketIsOwnerOnly(t *testing.T) {
	path := shortSocket(t)
	ln, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("права сокета %o, ожидались 600", perm)
	}
}

// Осиротевший сокет после падения процесса не должен мешать запуску, но и
// сносить не-сокет по этому пути нельзя.
func TestListenReplacesStaleSocketOnly(t *testing.T) {
	path := shortSocket(t)
	dir := filepath.Dir(path)

	ln, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	ln.Close() // файл остаётся, как после падения процесса
	if _, err := os.Stat(path); err != nil {
		t.Skip("сокет удалён при закрытии — проверять нечего")
	}
	ln2, err := Listen(path)
	if err != nil {
		t.Fatalf("осиротевший сокет не дал запуститься: %v", err)
	}
	ln2.Close()

	regular := filepath.Join(dir, "important.txt")
	if err := os.WriteFile(regular, []byte("данные"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(regular); err == nil {
		t.Error("обычный файл по пути сокета был бы снесён")
	}
	if _, err := os.Stat(regular); err != nil {
		t.Error("обычный файл всё-таки удалён")
	}
}

// Слишком длинный путь объясняется человеку, а не приходит как «bind: invalid
// argument» из ядра. Папка данных бывает глубокой, и это не экзотика.
func TestSocketPathLimitExplained(t *testing.T) {
	long := "/tmp/" + strings.Repeat("d/", 60) + "agent.sock"
	_, err := Listen(long)
	if err == nil {
		t.Fatal("длинный путь принят")
	}
	if !strings.Contains(err.Error(), "короче") {
		t.Errorf("сообщение не подсказывает выход: %v", err)
	}
	if _, err := Dial(long); err == nil || !strings.Contains(err.Error(), "короче") {
		t.Errorf("подключение по длинному пути: %v", err)
	}
}

// Для глубокой папки данных путь сокета уводится в короткий, но остаётся
// устойчивым: перезапуск находит свой сокет, а две установки не сталкиваются.
func TestSocketPathFallsBackForDeepDataDir(t *testing.T) {
	shallow := "/tmp/orch-data"
	if got := SocketPath(shallow); got != shallow+"/agent.sock" {
		t.Errorf("для короткой папки путь уведён зря: %s", got)
	}

	deep := "/Users/someone/Library/Application Support/very/deep/" + strings.Repeat("nested/", 8) + "data"
	got := SocketPath(deep)
	if len(got) > maxSocketPath {
		t.Errorf("запасной путь всё ещё длинный: %d байт", len(got))
	}
	if got == SocketPath(deep+"-other") {
		t.Error("две разные папки данных получили один сокет")
	}
	if got != SocketPath(deep) {
		t.Error("путь неустойчив между вызовами")
	}
}

// Отправка молчащему собеседнику обязана закончиться ошибкой, а не висеть
// вечно: иначе один зависший исполнитель подвесил бы рассылку заданий целиком.
func TestSendToSilentPeerFails(t *testing.T) {
	old := writeTimeout
	writeTimeout = 150 * time.Millisecond
	defer func() { writeTimeout = old }()

	a, b, stop := socketPair(t)
	defer stop()
	_ = b // собеседник существует, но ничего не читает

	big := make([]byte, 4<<20)
	for i := range big {
		big[i] = 'x'
	}
	body, _ := json.Marshal(string(big))

	done := make(chan error, 1)
	go func() {
		var err error
		// Первые посылки уходят в буфер ядра; ошибка приходит, когда он полон.
		for i := 0; i < 10 && err == nil; i++ {
			err = a.Send(&Envelope{Type: MsgEvent, Body: body})
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("отправка молчащему собеседнику прошла без ошибки")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("отправка зависла на молчащем собеседнике")
	}
}
