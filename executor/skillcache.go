package executor

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/realkasparov/orchestra-tennant/protocol"
)

// SkillCache — скиллы по хэшу на машине исполнителя: <dir>/<hash>/SKILL.md и
// orchestra.yaml. Скилл идентифицируется хэшем, а не именем: две таски с
// разными версиями одного скилла лежат рядом и не мешают друг другу. Для
// прогона скилл выкладывается в папку таски, откуда его читает агент.
type SkillCache struct {
	dir string

	mu        sync.Mutex
	manifests map[string]*protocol.Manifest // hash → разобранный манифест
	builtin   map[string]string             // имя → хэш встроенного скилла
}

// NewSkillCache открывает кэш в dir, создавая папку.
func NewSkillCache(dir string) (*SkillCache, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &SkillCache{dir: dir, manifests: map[string]*protocol.Manifest{}, builtin: map[string]string{}}, nil
}

func (c *SkillCache) path(hash string) string { return filepath.Join(c.dir, hash) }

// Has — скилл с таким хэшем лежит в кэше целиком.
func (c *SkillCache) Has(hash string) bool {
	if hash == "" {
		return false
	}
	_, err1 := os.Stat(filepath.Join(c.path(hash), "SKILL.md"))
	_, err2 := os.Stat(filepath.Join(c.path(hash), "orchestra.yaml"))
	return err1 == nil && err2 == nil
}

// Put кладёт скилл в кэш, проверив хэш содержимого и манифест.
func (c *SkillCache) Put(hash, name string, skillMD, manifest []byte) error {
	if got := protocol.SkillHash(skillMD, manifest); got != hash {
		return fmt.Errorf("скилл %s повреждён при доставке: хэш %s вместо %s", name, got[:12], hash[:min(12, len(hash))])
	}
	m, err := protocol.ParseManifest(manifest)
	if err != nil {
		return fmt.Errorf("скилл %s: %w", name, err)
	}
	if m.Name != name {
		return fmt.Errorf("скилл %s: в манифесте имя %s", name, m.Name)
	}
	dir := c.path(hash)
	tmp := dir + ".tmp"
	_ = os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(tmp, "SKILL.md"), skillMD, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(tmp, "orchestra.yaml"), manifest, 0o644); err != nil {
		return err
	}
	_ = os.RemoveAll(dir)
	if err := os.Rename(tmp, dir); err != nil {
		return err
	}
	c.mu.Lock()
	c.manifests[hash] = m
	c.mu.Unlock()
	return nil
}

// Manifest — манифест скилла из кэша.
func (c *SkillCache) Manifest(hash string) (*protocol.Manifest, error) {
	c.mu.Lock()
	m := c.manifests[hash]
	c.mu.Unlock()
	if m != nil {
		return m, nil
	}
	raw, err := os.ReadFile(filepath.Join(c.path(hash), "orchestra.yaml"))
	if err != nil {
		return nil, fmt.Errorf("скилла %s нет в кэше", hash[:min(12, len(hash))])
	}
	m, err = protocol.ParseManifest(raw)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.manifests[hash] = m
	c.mu.Unlock()
	return m, nil
}

// Hashes — все хэши в кэше, для hello.
func (c *SkillCache) Hashes() []string {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && c.Has(e.Name()) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// InstallTo выкладывает скилл в <taskDir>/.claude/skills/<name>/: агент
// получает папку таски через --add-dir и видит скилл именно оттуда.
func (c *SkillCache) InstallTo(taskDir, hash, name string) error {
	if !c.Has(hash) {
		return fmt.Errorf("скилла %s (%s) нет в кэше", name, hash[:min(12, len(hash))])
	}
	dst := filepath.Join(taskDir, ".claude", "skills", name)
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	for _, f := range []string{"SKILL.md", "orchestra.yaml"} {
		data, err := os.ReadFile(filepath.Join(c.path(hash), f))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dst, f), data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// SeedEmbedded кладёт встроенные скиллы (skills/<name>/{SKILL.md,orchestra.yaml})
// в кэш и запоминает их хэши по имени: по ним переводятся планы схемы 1.
func (c *SkillCache) SeedEmbedded(embedded fs.FS) error {
	entries, err := fs.ReadDir(embedded, "skills")
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		md, err := fs.ReadFile(embedded, "skills/"+name+"/SKILL.md")
		if err != nil {
			return fmt.Errorf("скилл %s не встроен: %w", name, err)
		}
		man, err := fs.ReadFile(embedded, "skills/"+name+"/orchestra.yaml")
		if err != nil {
			return fmt.Errorf("скилл %s без манифеста: %w", name, err)
		}
		hash := protocol.SkillHash(md, man)
		if !c.Has(hash) {
			if err := c.Put(hash, name, md, man); err != nil {
				return err
			}
		}
		c.mu.Lock()
		c.builtin[name] = hash
		c.mu.Unlock()
	}
	return nil
}

// BuiltinHashes — хэши встроенных скиллов по имени.
func (c *SkillCache) BuiltinHashes() map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]string, len(c.builtin))
	for k, v := range c.builtin {
		out[k] = v
	}
	return out
}
