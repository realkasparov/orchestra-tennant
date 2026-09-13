// Package codeindex — локальный индекс кода проекта: чанкование по
// синтаксическим границам, эмбеддинги локальной моделью и гибридный поиск.
// Индекс живёт рядом с данными проекта и служит инструментом `code_search`,
// который агент зовёт сам, когда grep не справляется.
package codeindex

import (
	"path/filepath"
	"strings"

	"github.com/realkasparov/orchestra-tennant/repomap"
)

const (
	// maxChunkBytes — потолок одного чанка. Длинные функции режутся на части:
	// эмбеддинг «размывается» на большом тексте и теряет точность.
	maxChunkBytes = 2500
	// minChunkBytes — куски мельче склеиваются с соседними: отдельный вектор
	// на трёхстрочный геттер только зашумляет выдачу.
	minChunkBytes = 120
	// preambleBytes — сколько байт начала файла (package/import/шапка) идёт в
	// контекстный заголовок каждого чанка.
	preambleBytes = 400
)

// Chunk — фрагмент файла с контекстом, пригодный для эмбеддинга.
type Chunk struct {
	Path      string
	StartLine int // 1-based, включительно
	EndLine   int
	Symbols   []string // объявления, попавшие в чанк
	Body      string
	preamble  string
}

// Text — то, что реально идёт в эмбеддинг: контекстный заголовок + тело.
// Заголовок (путь, окружение, имена объявлений) критичен — по исследованию
// contextual retrieval он даёт десятки процентов к точности поиска, потому
// что голый фрагмент кода не говорит, откуда он.
func (c Chunk) Text() string {
	var sb strings.Builder
	sb.WriteString("file: ")
	sb.WriteString(c.Path)
	if len(c.Symbols) > 0 {
		sb.WriteString("\ndeclares: ")
		sb.WriteString(strings.Join(c.Symbols, ", "))
	}
	if c.preamble != "" {
		sb.WriteString("\ncontext: ")
		sb.WriteString(c.preamble)
	}
	sb.WriteString("\n---\n")
	sb.WriteString(c.Body)
	return sb.String()
}

// ChunkFile режет файл по границам объявлений. Файлы без распознанных
// объявлений (в том числе markdown) режутся по размеру с сохранением строк.
func ChunkFile(path string, data []byte) []Chunk {
	if len(data) == 0 {
		return nil
	}
	src := string(data)
	preamble := makePreamble(path, src)

	decls := repomap.Decls(path, data)
	bounds := declBounds(decls, len(src))
	if len(bounds) == 0 {
		bounds = sizeBounds(src)
	}

	var out []Chunk
	for _, b := range bounds {
		body := src[b.start:b.end]
		if strings.TrimSpace(body) == "" {
			continue
		}
		for _, piece := range splitLong(body) {
			out = append(out, Chunk{
				Path:      path,
				StartLine: lineAt(src, b.start+piece.offset),
				EndLine:   lineAt(src, b.start+piece.offset+len(piece.text)-1),
				Symbols:   b.symbols,
				Body:      piece.text,
				preamble:  preamble,
			})
		}
	}
	return out
}

type bound struct {
	start, end int
	symbols    []string
}

// declBounds превращает список объявлений в отрезки [объявление, следующее).
// Всё, что до первого объявления (пакет, импорты), становится нулевым чанком —
// он часто отвечает на вопрос «что это за файл».
func declBounds(decls []repomap.Decl, size int) []bound {
	if len(decls) == 0 {
		return nil
	}
	var out []bound
	if decls[0].Offset > minChunkBytes {
		out = append(out, bound{start: 0, end: decls[0].Offset})
	}
	for i := 0; i < len(decls); {
		start := decls[i].Offset
		if i == 0 && len(out) == 0 {
			start = 0 // короткая шапка приклеивается к первому объявлению
		}
		syms := []string{decls[i].Name}
		end := size
		if i+1 < len(decls) {
			end = decls[i+1].Offset
		}
		// Соседние объявления, стоящие вплотную (однострочные type/const),
		// склеиваем, чтобы не плодить вектора на две строки.
		for end-start < minChunkBytes && i+1 < len(decls) {
			i++
			syms = append(syms, decls[i].Name)
			if i+1 < len(decls) {
				end = decls[i+1].Offset
			} else {
				end = size
			}
		}
		out = append(out, bound{start: start, end: end, symbols: syms})
		i++
	}
	return out
}

// sizeBounds режет файл без распознанных объявлений по строкам.
func sizeBounds(src string) []bound {
	var out []bound
	start := 0
	for pos := 0; pos < len(src); {
		nl := strings.IndexByte(src[pos:], '\n')
		if nl < 0 {
			pos = len(src)
		} else {
			pos += nl + 1
		}
		if pos-start >= maxChunkBytes || pos >= len(src) {
			out = append(out, bound{start: start, end: pos})
			start = pos
		}
	}
	return out
}

type piece struct {
	offset int
	text   string
}

// splitLong режет слишком длинный фрагмент по границам строк.
func splitLong(body string) []piece {
	if len(body) <= maxChunkBytes {
		return []piece{{offset: 0, text: body}}
	}
	var out []piece
	start := 0
	for pos := 0; pos < len(body); {
		nl := strings.IndexByte(body[pos:], '\n')
		if nl < 0 {
			pos = len(body)
		} else {
			pos += nl + 1
		}
		if pos-start >= maxChunkBytes || pos >= len(body) {
			out = append(out, piece{offset: start, text: body[start:pos]})
			start = pos
		}
	}
	return out
}

// makePreamble берёт шапку файла (package/import/заголовок) для контекста.
func makePreamble(path, src string) string {
	limit := len(src)
	if limit > preambleBytes {
		limit = preambleBytes
	}
	head := src[:limit]
	// Однострочная сводка: переносы съедают бюджет, а смысл несут слова.
	head = strings.Join(strings.Fields(head), " ")
	if len(head) > 200 {
		head = head[:200]
	}
	if strings.TrimSpace(head) == "" {
		return filepath.Dir(path)
	}
	return head
}

// lineAt возвращает 1-based номер строки для байтового смещения.
func lineAt(src string, offset int) int {
	if offset > len(src) {
		offset = len(src)
	}
	return strings.Count(src[:offset], "\n") + 1
}
