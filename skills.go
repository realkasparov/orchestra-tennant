// Package tennant — исполнитель тасок оркестратора Orchestra: то, что
// ставится на машину. Здесь корень модуля: встроенные скиллы этапов, чтобы
// демон на любой машине не зависел от рабочей копии репозитория.
package tennant

import "embed"

// Skills — скиллы этапов: skills/<name>/SKILL.md и orchestra.yaml.
//
//go:embed skills/*/SKILL.md skills/*/orchestra.yaml
var Skills embed.FS
