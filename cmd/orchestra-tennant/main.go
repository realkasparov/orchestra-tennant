// orchestra-tennant — автономный исполнитель: тот же internal/executor, что
// встроен в оркестратор, но отдельным процессом с собственной настройкой,
// журналом и фоновой службой. Второй реализации исполнителя здесь нет и быть
// не должно: если демон требует чего-то, чего не даёт протокол, чинится
// протокол.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/realkasparov/orchestra-tennant/executor"
	"github.com/realkasparov/orchestra-tennant/mcpserver"
)

const usage = `orchestra-tennant — исполнитель тасок оркестратора на этой машине.

Команды:
  setup     настроить: оркестратор, ключ подключения, модели, число заданий; самопроверки; служба
  run       работать (в терминале; служба вызывает то же самое)
  status    настроен ли, идёт ли служба, на связи ли демон
  logs      показать журнал; logs -f — следить
  service   install | uninstall — фоновая служба без повторной настройки
  version   версия

Общие флаги:
  -home DIR  папка демона (по умолчанию $ORCHESTRA_TENNANT_HOME или ~/.orchestra-tennant)
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "-mcp-codesearch":
		// Агент зовёт этот же бинарь как MCP-сервер code_search (см.
		// Pipeline.codeSearchFor): у демона свой индекс в своей папке.
		err = runMCP(os.Args[1:])
	case "setup":
		err = cmdSetup(args)
	case "run":
		err = cmdRun(args)
	case "status":
		err = cmdStatus(args)
	case "logs":
		err = cmdLogs(args)
	case "service":
		err = cmdService(args)
	case "version", "-v", "--version":
		fmt.Println("orchestra-tennant", versionString())
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "неизвестная команда %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ошибка:", err)
		os.Exit(1)
	}
}

// flagsFor заводит набор флагов подкоманды с общим -home.
func flagsFor(name string) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet("orchestra-tennant "+name, flag.ExitOnError)
	home := fs.String("home", defaultHome(), "папка демона")
	return fs, home
}

func defaultHome() string {
	if h := os.Getenv("ORCHESTRA_TENNANT_HOME"); h != "" {
		return h
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".orchestra-tennant"
	}
	return filepath.Join(home, ".orchestra-tennant")
}

// signalContext отменяется по SIGINT/SIGTERM: так служба останавливает демона,
// и исполнитель успевает записать журнал заданий.
func signalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		select {
		case <-ch:
			cancel()
		case <-ctx.Done():
		}
		signal.Stop(ch)
	}()
	return ctx, cancel
}

func versionString() string { return executor.Version }

// runMCP — режим stdio-сервера code_search над индексом демона.
func runMCP(args []string) error {
	fs := flag.NewFlagSet("orchestra-tennant -mcp-codesearch", flag.ExitOnError)
	_ = fs.Bool("mcp-codesearch", false, "")
	data := fs.String("mcp-data", "", "папка с индексом кода")
	project := fs.Int64("mcp-project", 0, "проект")
	root := fs.String("mcp-root", "", "корень проекта")
	_ = fs.Parse(args)
	idx, err := mcpserver.OpenIndex(*data, *project, *root)
	if err != nil {
		return err
	}
	defer idx.Close()
	return mcpserver.Serve(os.Stdin, os.Stdout, idx)
}
