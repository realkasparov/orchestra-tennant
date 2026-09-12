package executor

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Version — версия исполнителя. Одна на встроенный и внешний: расхождение
// видно на карточке устройства, а не угадывается. Переменная, а не
// константа: релизная сборка подставляет тег через -ldflags -X.
var Version = "0.3.0"

// StageSkills — скиллы, которые зовут этапы плана. Исполнитель объявляет их в
// hello, и оркестратор не предложит ему план с незнакомым скиллом.
var StageSkills = []string{"import-gitlab", "analyze-task", "plan-review", "execute-plan", "review-task"}

// InstallSkills делает скиллы из src видимыми headless-запускам claude:
// симлинки в ~/.claude/skills. Это работа исполнителя, а не оркестратора —
// скиллы нужны на той машине, где запускается агент.
func InstallSkills(src, home string) error {
	abs, err := filepath.Abs(src)
	if err != nil {
		return err
	}
	dstRoot := filepath.Join(home, ".claude", "skills")
	if err := os.MkdirAll(dstRoot, 0o755); err != nil {
		return err
	}
	for _, name := range StageSkills {
		from := filepath.Join(abs, name)
		if _, err := os.Stat(from); err != nil {
			return err
		}
		to := filepath.Join(dstRoot, name)
		if existing, err := os.Readlink(to); err == nil && existing == from {
			continue
		}
		_ = os.Remove(to)
		if err := os.Symlink(from, to); err != nil {
			return err
		}
	}
	return nil
}

// MaterializeSkills выкладывает встроенные скиллы (skills/<name>/SKILL.md) в
// dir и возвращает папку, которую можно отдать InstallSkills. Файлы
// перезаписываются: после обновления бинаря скиллы должны совпадать с ним.
func MaterializeSkills(embedded fs.FS, dir string) (string, error) {
	root := filepath.Join(dir, "skills")
	for _, name := range StageSkills {
		data, err := fs.ReadFile(embedded, "skills/"+name+"/SKILL.md")
		if err != nil {
			return "", fmt.Errorf("скилл %s не встроен: %w", name, err)
		}
		if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(root, name, "SKILL.md"), data, 0o644); err != nil {
			return "", err
		}
	}
	return root, nil
}
