package codeindex

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"time"
)

const (
	// DefaultModel — локальная модель эмбеддингов. Qwen3-Embedding обучена с
	// MRL, поэтому усечение вектора до Dims не ломает геометрию.
	DefaultModel = "qwen3-embedding:0.6b"
	// Dims — рабочая размерность после MRL-усечения. 256 хватает для поиска
	// по коду и даёт 4-кратную экономию памяти и времени перебора.
	Dims = 256

	embedTimeout = 2 * time.Minute
	batchSize    = 32
)

// Embedder считает эмбеддинги локальной моделью через Ollama.
type Embedder struct {
	BaseURL string
	Model   string
	client  *http.Client
}

// NewEmbedder создаёт клиента; адрес берётся из OLLAMA_HOST, если задан.
func NewEmbedder(model string) *Embedder {
	base := os.Getenv("OLLAMA_HOST")
	if base == "" {
		base = "http://127.0.0.1:11434"
	}
	if model == "" {
		model = DefaultModel
	}
	return &Embedder{BaseURL: base, Model: model, client: &http.Client{Timeout: embedTimeout}}
}

// Available проверяет, что Ollama отвечает и нужная модель установлена.
// Возвращаемая ошибка предназначена пользователю — она объясняет, что делать.
func (e *Embedder) Available(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.BaseURL+"/api/tags", nil)
	if err != nil {
		return err
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return fmt.Errorf("Ollama недоступна по %s (индексация кода отключена): %w", e.BaseURL, err)
	}
	defer resp.Body.Close()
	var tags struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil {
		return err
	}
	for _, m := range tags.Models {
		if m.Name == e.Model || m.Name == e.Model+":latest" {
			return nil
		}
	}
	return fmt.Errorf("модель %s не установлена — выполните `ollama pull %s`", e.Model, e.Model)
}

// queryInstruct — префикс запроса. Qwen3-Embedding обучена асимметрично:
// документы эмбеддятся как есть, а запрос обязан нести инструкцию задачи.
// Без неё запрос и документ попадают в разные области пространства, и на
// абстрактных формулировках выдача заметно деградирует.
const queryInstruct = "Instruct: Given a question about a codebase, retrieve the code fragments that implement or answer it\nQuery: "

// EmbedQuery считает вектор поискового запроса (с инструкцией).
func (e *Embedder) EmbedQuery(ctx context.Context, query string) ([]float32, error) {
	vecs, err := e.Embed(ctx, []string{queryInstruct + query})
	if err != nil {
		return nil, err
	}
	if len(vecs) == 0 {
		return nil, fmt.Errorf("пустой ответ модели на запрос")
	}
	return vecs[0], nil
}

// Embed считает векторы для батча текстов. Порядок результата соответствует
// порядку входа.
func (e *Embedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += batchSize {
		end := start + batchSize
		if end > len(texts) {
			end = len(texts)
		}
		vecs, err := e.embedBatch(ctx, texts[start:end])
		if err != nil {
			return nil, err
		}
		out = append(out, vecs...)
	}
	return out, nil
}

func (e *Embedder) embedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	body, _ := json.Marshal(map[string]any{"model": e.Model, "input": texts})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.BaseURL+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Ollama /api/embed: HTTP %d", resp.StatusCode)
	}
	var parsed struct {
		Embeddings [][]float64 `json:"embeddings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}
	if len(parsed.Embeddings) != len(texts) {
		return nil, fmt.Errorf("Ollama вернула %d векторов на %d текстов", len(parsed.Embeddings), len(texts))
	}
	out := make([][]float32, len(parsed.Embeddings))
	for i, v := range parsed.Embeddings {
		out[i] = truncateNormalize(v)
	}
	return out, nil
}

// truncateNormalize урезает вектор до Dims (MRL) и нормирует его, чтобы
// косинусная мера сводилась к скалярному произведению.
func truncateNormalize(v []float64) []float32 {
	n := Dims
	if len(v) < n {
		n = len(v)
	}
	out := make([]float32, Dims)
	var norm float64
	for i := 0; i < n; i++ {
		norm += v[i] * v[i]
	}
	norm = math.Sqrt(norm)
	if norm == 0 {
		return out
	}
	for i := 0; i < n; i++ {
		out[i] = float32(v[i] / norm)
	}
	return out
}

// encodeVec упаковывает вектор в BLOB (float32 little-endian).
func encodeVec(v []float32) []byte {
	buf := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(f))
	}
	return buf
}

// decodeVec распаковывает BLOB обратно в вектор.
func decodeVec(b []byte) []float32 {
	out := make([]float32, len(b)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return out
}

// dot — скалярное произведение; для нормированных векторов это косинус.
func dot(a, b []float32) float32 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	var sum float32
	for i := 0; i < n; i++ {
		sum += a[i] * b[i]
	}
	return sum
}
