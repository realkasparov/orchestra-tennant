package protocol

import (
	"fmt"
	"strings"
)

// CheckRef — защита имени ветки или ref: ведущий «-» git прочитал бы как
// опцию (подстановка аргумента). Проверяется и в оркестраторе при вводе, и
// в исполнителе перед каждым вызовом git.
func CheckRef(name string) error {
	if name == "" || strings.HasPrefix(name, "-") {
		return fmt.Errorf("недопустимое имя ветки/ref: %q", name)
	}
	return nil
}
