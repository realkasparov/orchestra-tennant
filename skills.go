// Package tennant — исполнитель тасок оркестратора Orchestra: то, что
// ставится на машину. Здесь корень модуля: встроенные скиллы этапов, чтобы
// демон на любой машине не зависел от рабочей копии репозитория.
package tennant

import "embed"

// Skills — скиллы этапов: skills/<name>/SKILL.md.
//
//go:embed skills/*/SKILL.md
var Skills embed.FS
