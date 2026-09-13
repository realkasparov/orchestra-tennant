package protocol

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ErrClosed возвращается при работе с закрытым соединением.
var ErrClosed = errors.New("соединение закрыто")

// Conn — канал обмена сообщениями между оркестратором и исполнителем.
//
// Протокол не должен зависеть от способа доставки: сегодня исполнитель живёт в
// том же процессе, завтра — отдельным демоном на этой машине, послезавтра — на
// другой. Поэтому здесь ровно три операции, и обе реализации проверяются одним
// набором тестов: иначе сокет доехал бы до первого демона неопробованным.
//
// Send безопасен из нескольких горутин. Recv рассчитан на одного читателя:
// сообщения приходят потоком, и делить его между читателями бессмысленно.
type Conn interface {
	Send(*Envelope) error
	Recv() (*Envelope, error)
	Close() error
}

// --- транспорт внутри процесса ---

// Pipe соединяет две стороны внутри одного процесса. Так подключается
// встроенный исполнитель: гонять его сообщения через сокет на той же машине
// значило бы сериализовать и разбирать JSON ради путешествия из процесса в
// него же.
func Pipe() (Conn, Conn) {
	ab := make(chan *Envelope, 64)
	ba := make(chan *Envelope, 64)
	done := make(chan struct{})
	var once sync.Once
	closeBoth := func() { once.Do(func() { close(done) }) }
	return &pipeConn{out: ab, in: ba, done: done, shut: closeBoth},
		&pipeConn{out: ba, in: ab, done: done, shut: closeBoth}
}

type pipeConn struct {
	out  chan<- *Envelope
	in   <-chan *Envelope
	done chan struct{}
	shut func()
}

func (c *pipeConn) Send(e *Envelope) error {
	// Отправка не должна вечно висеть на закрытом соединении: выбор между
	// каналом и done решает это без гонки с закрытием.
	select {
	case <-c.done:
		return ErrClosed
	default:
	}
	select {
	case c.out <- e:
		return nil
	case <-c.done:
		return ErrClosed
	}
}

func (c *pipeConn) Recv() (*Envelope, error) {
	select {
	case e := <-c.in:
		return e, nil
	case <-c.done:
		// Уже пришедшее дочитываем: закрытие не должно терять сообщение,
		// которое отправитель считает доставленным.
		select {
		case e := <-c.in:
			return e, nil
		default:
			return nil, io.EOF
		}
	}
}

func (c *pipeConn) Close() error { c.shut(); return nil }

// --- транспорт через сокет ---

// maxSocketPath — предел длины пути Unix-сокета. Ядро хранит его в поле
// фиксированного размера: 104 байта на macOS и BSD, 108 на Linux. Берём
// меньшее с запасом на завершающий ноль — ошибка от превышения приходит как
// невнятное «bind: invalid argument», и ловить её в проде дорого.
const maxSocketPath = 100

// SocketPath — путь к сокету для данной папки данных.
//
// Обычно сокет лежит рядом с базой: его видно, он уходит вместе с папкой. Но
// путь к папке данных бывает глубоким, а предел на длину — жёсткий, поэтому
// для длинных путей берётся короткий и устойчивый путь во временной папке,
// выведенный из самого пути данных: две копии сервиса с разными папками не
// столкнутся, а перезапуск одной и той же найдёт свой сокет.
func SocketPath(dataDir string) string {
	direct := filepath.Join(dataDir, "agent.sock")
	if len(direct) <= maxSocketPath {
		return direct
	}
	// Своя папка, а не файл прямо в /tmp: между созданием сокета и chmod он
	// на мгновение доступен всем, а /tmp читает кто угодно. Папка 0700
	// закрывает это окно — её создаёт Listen.
	sum := sha256.Sum256([]byte(dataDir))
	return filepath.Join("/tmp", "orchestra-"+hex.EncodeToString(sum[:6]), "agent.sock")
}

// Listen открывает Unix-сокет для исполнителей.
//
// Локально аутентификация — это права файла: сокет доступен только владельцу,
// а владелец сервиса и владелец исполнителя — один человек. TLS здесь защищал
// бы трафик машины от неё самой.
func Listen(path string) (net.Listener, error) {
	if err := checkSocketPath(path); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	// Осиротевший сокет остаётся после падения процесса и не даёт занять адрес.
	// Удаляем только то, что действительно сокет: снести обычный файл, на
	// который кто-то указал по ошибке, недопустимо.
	if fi, err := os.Stat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, errors.New("путь занят не сокетом: " + path)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	// Права ставим после Listen: до создания файла менять нечего, а зонтик
	// umask мог оставить сокет доступным другим пользователям машины.
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// checkSocketPath объясняет превышение предела вместо «bind: invalid argument».
func checkSocketPath(path string) error {
	if len(path) > maxSocketPath {
		return fmt.Errorf("путь сокета длиннее %d байт (%d): %s — выберите папку данных короче",
			maxSocketPath, len(path), path)
	}
	return nil
}

// Dial подключается к сокету оркестратора.
func Dial(path string) (Conn, error) {
	if err := checkSocketPath(path); err != nil {
		return nil, err
	}
	c, err := net.Dial("unix", path)
	if err != nil {
		return nil, err
	}
	return NewConn(c), nil
}

// writeTimeout — сколько ждать, пока собеседник разберёт буфер сокета.
//
// Запись в сокет блокирующая: буфер ядра невелик (десятки килобайт), и план на
// три сотни килобайт уходит только по мере чтения на той стороне. Исполнитель,
// который перестал читать — завис, ушёл в своп, попал в отладчик, — иначе
// подвесил бы отправляющую горутину оркестратора навсегда, и вместе с ней всю
// рассылку заданий. Срок щедрый: это не «медленно», а «мёртв».
//
// Переменная, а не константа, только ради тестов: проверять поведение с
// тридцатисекундной паузой в наборе тестов невозможно.
var writeTimeout = 30 * time.Second

// deadlineSetter — то, что умеет ограничивать время записи (любой net.Conn).
type deadlineSetter interface{ SetWriteDeadline(time.Time) error }

// NewConn оборачивает готовое соединение. Кадрирование — поток JSON-значений
// подряд: json.Decoder читает их сам, без ограничения на длину, в отличие от
// построчного чтения, где план или крупное событие упёрлись бы в предел буфера.
func NewConn(rwc io.ReadWriteCloser) Conn {
	c := &streamConn{rwc: rwc, dec: json.NewDecoder(rwc), enc: json.NewEncoder(rwc)}
	c.dl, _ = rwc.(deadlineSetter)
	return c
}

type streamConn struct {
	rwc io.ReadWriteCloser
	dec *json.Decoder
	enc *json.Encoder
	dl  deadlineSetter
	mu  sync.Mutex // Send зовут из разных горутин, а запись должна быть целой
	off bool
}

func (c *streamConn) Send(e *Envelope) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.off {
		return ErrClosed
	}
	if c.dl != nil {
		if err := c.dl.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
			return err
		}
		defer c.dl.SetWriteDeadline(time.Time{}) //nolint:errcheck // снятие срока
	}
	return c.enc.Encode(e)
}

func (c *streamConn) Recv() (*Envelope, error) {
	var e Envelope
	if err := c.dec.Decode(&e); err != nil {
		return nil, err
	}
	return &e, nil
}

func (c *streamConn) Close() error {
	c.mu.Lock()
	c.off = true
	c.mu.Unlock()
	return c.rwc.Close()
}
