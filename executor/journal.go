// Package executor — исполняющая сторона: подключается к оркестратору,
// принимает задания по протоколу и ведёт их по этапам. На этапе 2 живёт внутри
// процесса оркестратора, на этапе 3 — отдельным демоном, тем же кодом.
package executor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/realkasparov/orchestra-tennant/protocol"
)

// Journal — что исполнитель ведёт прямо сейчас, на диске.
//
// Нужен, чтобы пережить собственный перезапуск: после него исполнитель
// перечисляет незавершённые задания в hello, оркестратор сверяет их с
// закреплением и возвращает с resume. Без журнала упавший исполнитель молча
// терял бы задание, а оркестратор ждал бы его таймаут этапа впустую.
type Journal struct{ dir string }

// Record — запись о задании.
type Record struct {
	JobID  string         `json:"job_id"`
	TaskID int64          `json:"task_id"`
	Plan   *protocol.Plan `json:"plan"`
	Resume bool           `json:"resume"`
	// LastAck — до какого номера оркестратор подтвердил события. Всё новее
	// после переподключения досылается.
	LastAck int64 `json:"last_ack"`
	// State — состояние таски у исполнителя: этапы, сессии агента, вопросы.
	// Без него после перезапуска возобновление начинало бы этап заново, не
	// зная сессии, которую можно продолжить.
	State *TaskState `json:"state,omitempty"`
}

// OpenJournal открывает журнал в папке, создавая её только для владельца.
func OpenJournal(dir string) (*Journal, error) {
	if dir == "" {
		return nil, fmt.Errorf("папка журнала не задана: без неё задание не переживёт перезапуск")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Journal{dir: dir}, nil
}

func (j *Journal) path(jobID string) string {
	// Идентификатор задания — hex от оркестратора, но доверять этому при
	// сборке пути нельзя: журнал должен оставаться внутри своей папки, что бы
	// ни пришло по проводу.
	safe := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r >= 'A' && r <= 'Z' || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, jobID)
	return filepath.Join(j.dir, safe+".json")
}

// Put записывает состояние задания. Запись атомарная — через временный файл и
// переименование: полузаписанный журнал после сбоя питания хуже отсутствующего.
func (j *Journal) Put(r *Record) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	final := j.path(r.JobID)
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, final)
}

// Remove стирает запись завершённого задания.
func (j *Journal) Remove(jobID string) error {
	err := os.Remove(j.path(jobID))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// List возвращает незавершённые задания. Повреждённая запись пропускается с
// ошибкой в списке, а не роняет весь список: одно битое задание не должно
// мешать остальным.
func (j *Journal) List() ([]*Record, []error) {
	entries, err := os.ReadDir(j.dir)
	if err != nil {
		return nil, []error{err}
	}
	var out []*Record
	var errs []error
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(j.dir, e.Name()))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		var r Record
		if err := json.Unmarshal(raw, &r); err != nil || r.JobID == "" || r.Plan == nil {
			errs = append(errs, fmt.Errorf("журнал %s повреждён: %v", e.Name(), err))
			continue
		}
		out = append(out, &r)
	}
	return out, errs
}
