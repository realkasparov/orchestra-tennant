package protocol

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Манифест скилла — orchestra.yaml рядом с SKILL.md: что скилл ждёт на входе,
// какой контекст ему считать на месте, что он отдаёт и как проверить, что он
// отработал. Без манифеста конструктор не может ни проверить совместимость
// шагов, ни исполнить скилл вслепую.

// ManifestVersion — поддерживаемая версия формата манифеста.
const ManifestVersion = 1

// Режимы рабочей копии скилла.
const (
	WorkspaceManaged = "managed" // движок готовит worktree и ветку
	WorkspaceSelf    = "self"    // скилл сам заводит ветку и коммитит
)

// Типы входов.
var knownInputTypes = map[string]bool{"text": true, "path": true, "git_ref": true, "url": true, "enum": true, "json": true}

// Провайдеры контекста, которые движок считает на месте.
var knownContext = map[string]bool{"repo_map": true, "plan": true, "files": true, "diff": true}

// Типы маркеров и артефактов.
var (
	knownMarkerTypes   = map[string]bool{"text": true, "enum": true, "git_branch": true}
	knownArtifactTypes = map[string]bool{"markdown": true, "json": true}
	knownChecks        = map[string]bool{"artifact_exists": true, "marker_present": true, "json_valid": true, "has_commits": true, "clean_tree": true}
)

// Manifest — разобранный orchestra.yaml.
type Manifest struct {
	Name    string `json:"name"`
	Title   string `json:"title"`
	Version int    `json:"version"`
	// Inputs — входы по имени; InputOrder — в порядке объявления: по нему
	// собирается промпт.
	Inputs     map[string]Input `json:"inputs"`
	InputOrder []string         `json:"input_order"`
	Context    []string         `json:"context,omitempty"`
	Workspace  string           `json:"workspace,omitempty"` // managed | self
	CWD        string           `json:"cwd,omitempty"`       // worktree | task_dir
	// Tools — инструменты Claude Code; пусто — без ограничений.
	Tools           []string `json:"tools,omitempty"`
	CodeSearch      bool     `json:"code_search,omitempty"`
	Questions       bool     `json:"questions,omitempty"`
	Resumable       bool     `json:"resumable,omitempty"`
	Outputs         Outputs  `json:"outputs"`
	Checks          []Check  `json:"checks,omitempty"`
	ModelPreference []string `json:"model_preference,omitempty"`
}

// Input — параметр промпта.
type Input struct {
	Type     string `json:"type"`
	Required bool   `json:"required"`
	Desc     string `json:"desc,omitempty"`
}

// Outputs — что скилл отдаёт.
type Outputs struct {
	Artifacts []Artifact        `json:"artifacts,omitempty"`
	Markers   map[string]Marker `json:"markers,omitempty"`
	// MarkerOrder — порядок объявления маркеров.
	MarkerOrder []string `json:"marker_order,omitempty"`
}

// Artifact — файл в папке задачи.
type Artifact struct {
	Name   string         `json:"name"`
	Title  string         `json:"title"`
	Type   string         `json:"type"` // markdown | json
	Schema map[string]any `json:"schema,omitempty"`
}

// Marker — однострочный сигнал в тексте агента.
type Marker struct {
	Type   string   `json:"type"` // text | enum | git_branch
	Values []string `json:"values,omitempty"`
}

// Check — пост-проверка после прогона.
type Check struct {
	Kind string `json:"kind"`
	// Arg — файл для artifact_exists/json_valid, маркер для marker_present и
	// has_commits (маркер с именем ветки).
	Arg string `json:"arg,omitempty"`
}

// ParseManifest разбирает и проверяет orchestra.yaml.
func ParseManifest(raw []byte) (*Manifest, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(raw, &root); err != nil {
		return nil, fmt.Errorf("orchestra.yaml не разобрать: %w", err)
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("orchestra.yaml: ожидается объект")
	}
	m := &Manifest{Inputs: map[string]Input{}, Outputs: Outputs{Markers: map[string]Marker{}}}
	top := root.Content[0]
	for i := 0; i+1 < len(top.Content); i += 2 {
		key, val := top.Content[i].Value, top.Content[i+1]
		var err error
		switch key {
		case "name":
			m.Name = val.Value
		case "title":
			m.Title = val.Value
		case "version":
			err = val.Decode(&m.Version)
		case "inputs":
			err = m.parseInputs(val)
		case "context":
			err = val.Decode(&m.Context)
		case "workspace":
			m.Workspace = val.Value
		case "cwd":
			m.CWD = val.Value
		case "tools":
			if val.Kind == yaml.ScalarNode {
				if val.Value != "unrestricted" {
					err = fmt.Errorf("tools: %q — ожидается unrestricted или список", val.Value)
				}
			} else {
				err = val.Decode(&m.Tools)
			}
		case "code_search":
			err = val.Decode(&m.CodeSearch)
		case "questions":
			err = val.Decode(&m.Questions)
		case "resumable":
			err = val.Decode(&m.Resumable)
		case "outputs":
			err = m.parseOutputs(val)
		case "checks":
			err = m.parseChecks(val)
		case "model_preference":
			err = val.Decode(&m.ModelPreference)
		default:
			err = fmt.Errorf("%s: неизвестное поле", key)
		}
		if err != nil {
			return nil, fmt.Errorf("orchestra.yaml: %w", err)
		}
	}
	if err := m.validate(); err != nil {
		return nil, fmt.Errorf("orchestra.yaml: %w", err)
	}
	return m, nil
}

var inputNameRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

func (m *Manifest) parseInputs(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("inputs: ожидается объект «ИМЯ: {type, required}»")
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		name := n.Content[i].Value
		var in Input
		if err := n.Content[i+1].Decode(&in); err != nil {
			return fmt.Errorf("inputs.%s: %w", name, err)
		}
		if !inputNameRe.MatchString(name) {
			return fmt.Errorf("inputs.%s: имя входа — заглавные латинские буквы, цифры и подчёркивание", name)
		}
		if !knownInputTypes[in.Type] {
			return fmt.Errorf("inputs.%s.type: неизвестный тип %q", name, in.Type)
		}
		m.Inputs[name] = in
		m.InputOrder = append(m.InputOrder, name)
	}
	return nil
}

func (m *Manifest) parseOutputs(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("outputs: ожидается объект")
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		key, val := n.Content[i].Value, n.Content[i+1]
		switch key {
		case "artifacts":
			if err := val.Decode(&m.Outputs.Artifacts); err != nil {
				return fmt.Errorf("outputs.artifacts: %w", err)
			}
		case "markers":
			if val.Kind != yaml.MappingNode {
				return fmt.Errorf("outputs.markers: ожидается объект «ИМЯ: {type}»")
			}
			for j := 0; j+1 < len(val.Content); j += 2 {
				name := val.Content[j].Value
				var mk Marker
				if err := val.Content[j+1].Decode(&mk); err != nil {
					return fmt.Errorf("outputs.markers.%s: %w", name, err)
				}
				m.Outputs.Markers[name] = mk
				m.Outputs.MarkerOrder = append(m.Outputs.MarkerOrder, name)
			}
		default:
			return fmt.Errorf("outputs.%s: неизвестное поле", key)
		}
	}
	return nil
}

func (m *Manifest) parseChecks(n *yaml.Node) error {
	if n.Kind != yaml.SequenceNode {
		return fmt.Errorf("checks: ожидается список")
	}
	for i, item := range n.Content {
		switch item.Kind {
		case yaml.ScalarNode:
			m.Checks = append(m.Checks, Check{Kind: item.Value})
		case yaml.MappingNode:
			if len(item.Content) != 2 {
				return fmt.Errorf("checks[%d]: одна проверка на строку", i)
			}
			kind, val := item.Content[0].Value, item.Content[1]
			c := Check{Kind: kind}
			switch val.Kind {
			case yaml.ScalarNode:
				c.Arg = val.Value
			case yaml.MappingNode:
				var args map[string]string
				if err := val.Decode(&args); err != nil {
					return fmt.Errorf("checks[%d]: %w", i, err)
				}
				c.Arg = args["branch"]
				if c.Arg == "" {
					c.Arg = args["file"]
				}
			}
			m.Checks = append(m.Checks, c)
		default:
			return fmt.Errorf("checks[%d]: непонятная запись", i)
		}
	}
	return nil
}

func (m *Manifest) validate() error {
	if !keyRe.MatchString(strings.ReplaceAll(m.Name, "-", "_")) {
		return fmt.Errorf("name: %q — латиница, цифры, дефис", m.Name)
	}
	if m.Version != ManifestVersion {
		return fmt.Errorf("version: %d, поддерживается %d", m.Version, ManifestVersion)
	}
	if strings.TrimSpace(m.Title) == "" {
		m.Title = m.Name
	}
	for _, c := range m.Context {
		if !knownContext[c] {
			return fmt.Errorf("context: неизвестный провайдер %q", c)
		}
	}
	switch m.Workspace {
	case "", WorkspaceManaged, WorkspaceSelf:
	default:
		return fmt.Errorf("workspace: %q — ожидается managed или self", m.Workspace)
	}
	switch m.CWD {
	case "", "worktree", "task_dir":
	default:
		return fmt.Errorf("cwd: %q — ожидается worktree или task_dir", m.CWD)
	}
	for i, a := range m.Outputs.Artifacts {
		if a.Name == "" || strings.ContainsAny(a.Name, "/\\") {
			return fmt.Errorf("outputs.artifacts[%d].name: имя файла без пути", i)
		}
		if a.Type == "" {
			a.Type = "markdown"
			m.Outputs.Artifacts[i].Type = a.Type
		}
		if !knownArtifactTypes[a.Type] {
			return fmt.Errorf("outputs.artifacts[%d].type: неизвестный тип %q", i, a.Type)
		}
		if a.Type == "json" {
			if a.Schema == nil {
				return fmt.Errorf("outputs.artifacts[%d].schema: обязательно для type json", i)
			}
			if err := CheckSchema(a.Schema); err != nil {
				return fmt.Errorf("outputs.artifacts[%d].schema: %w", i, err)
			}
		}
		if a.Title == "" {
			m.Outputs.Artifacts[i].Title = a.Name
		}
	}
	for name, mk := range m.Outputs.Markers {
		if !inputNameRe.MatchString(name) {
			return fmt.Errorf("outputs.markers.%s: имя маркера — заглавные латинские буквы", name)
		}
		if mk.Type == "" {
			mk.Type = "text"
			m.Outputs.Markers[name] = mk
		}
		if !knownMarkerTypes[mk.Type] {
			return fmt.Errorf("outputs.markers.%s.type: неизвестный тип %q", name, mk.Type)
		}
		if mk.Type == "enum" && len(mk.Values) == 0 {
			return fmt.Errorf("outputs.markers.%s.values: у enum нужны значения", name)
		}
	}
	for i, c := range m.Checks {
		if !knownChecks[c.Kind] {
			return fmt.Errorf("checks[%d]: неизвестная проверка %q", i, c.Kind)
		}
		switch c.Kind {
		case "artifact_exists", "json_valid":
			if c.Arg == "" {
				return fmt.Errorf("checks[%d]: %s без имени файла", i, c.Kind)
			}
			if m.artifact(c.Arg) == nil {
				return fmt.Errorf("checks[%d]: файл %s не объявлен в outputs.artifacts", i, c.Arg)
			}
			if c.Kind == "json_valid" && m.artifact(c.Arg).Type != "json" {
				return fmt.Errorf("checks[%d]: json_valid для файла не типа json", i)
			}
		case "marker_present", "has_commits":
			if c.Arg == "" {
				if c.Kind == "has_commits" {
					continue // без маркера: текущая ветка против базы раунда
				}
				return fmt.Errorf("checks[%d]: %s без имени маркера", i, c.Kind)
			}
			mk, ok := m.Outputs.Markers[c.Arg]
			if !ok {
				return fmt.Errorf("checks[%d]: маркер %s не объявлен", i, c.Arg)
			}
			if c.Kind == "has_commits" && mk.Type != "git_branch" {
				return fmt.Errorf("checks[%d]: has_commits ждёт маркер типа git_branch", i)
			}
		}
	}
	for _, k := range m.ModelPreference {
		if ModelID(k) == Models[0].ID && k != Models[0].Key && k != Models[0].ID {
			return fmt.Errorf("model_preference: неизвестная модель %q", k)
		}
	}
	return nil
}

func (m *Manifest) artifact(name string) *Artifact {
	for i := range m.Outputs.Artifacts {
		if m.Outputs.Artifacts[i].Name == name {
			return &m.Outputs.Artifacts[i]
		}
	}
	return nil
}

// Artifact — объявление файла по имени.
func (m *Manifest) Artifact(name string) *Artifact { return m.artifact(name) }

// ArtifactNames — имена файлов, которые скилл пишет.
func (m *Manifest) ArtifactNames() []string {
	out := make([]string, 0, len(m.Outputs.Artifacts))
	for _, a := range m.Outputs.Artifacts {
		out = append(out, a.Name)
	}
	return out
}

// SkillHash — хэш содержимого скилла: SKILL.md и orchestra.yaml вместе.
// Хэш и есть версия: изменился любой из файлов — новая запись.
func SkillHash(skillMD, manifest []byte) string {
	h := sha256.New()
	fmt.Fprintf(h, "SKILL.md\x00%d\x00", len(skillMD))
	h.Write(skillMD)
	fmt.Fprintf(h, "\x00orchestra.yaml\x00%d\x00", len(manifest))
	h.Write(manifest)
	return hex.EncodeToString(h.Sum(nil))
}

// SkillRef — скилл в плане: что докачать и проверить.
type SkillRef struct {
	Name string `json:"name"`
	Hash string `json:"hash"`
	Size int    `json:"size,omitempty"`
}

// SkillGet — запрос скилла у оркестратора.
type SkillGet struct {
	Hash string `json:"hash"`
}

// SkillFile — скилл целиком: два файла. Error — хэш неизвестен реестру.
type SkillFile struct {
	Hash     string `json:"hash"`
	Name     string `json:"name"`
	SkillMD  string `json:"skill_md"`
	Manifest string `json:"manifest"`
	Error    string `json:"error,omitempty"`
}

// ManifestJSON — манифест как JSON для хранения в базе и отдачи вебу.
func (m *Manifest) ManifestJSON() string {
	raw, _ := json.Marshal(m)
	return string(raw)
}
