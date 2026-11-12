// 1c-naparnik-mcp — MCP-сервер (stdio) для публичного API 1С:Напарника.
//
// Переменные окружения:
//
//	NAPARNIK_API_KEY    — API-ключ из раздела «Ключи доступа» (обязательно; задаётся только здесь)
//	NAPARNIK_BASE_URL   — базовый адрес API (по умолчанию https://code.1c.ai)
//	NAPARNIK_FILES_ROOT — каталоги, которые 1С:Напарник может читать, через ';' (необязательно)
//	NAPARNIK_WRITE_ROOT — каталоги, в которых 1С:Напарник может создавать и править файлы, через ';';
//	                      каждый должен быть внутри одного из каталогов чтения (необязательно)
//	NAPARNIK_CONFIG     — путь к cfg.json (по умолчанию cfg.json рядом с exe, если есть)
//	NAPARNIK_TOOLS      — путь к tools.json (по умолчанию tools.json рядом с cfg.json или exe, если есть)
//
// cfg.json — числа и списки (таймауты, лимиты, каталоги), tools.json — тексты для агента и 1С:Напарника.
// В обоих файлах указываются только изменяемые ключи; полный шаблон: -dump-config, -dump-tools.
// Переменные окружения важнее cfg.json.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"1c-naparnik-mcp/naparnik"
)

const serverName = "1c-naparnik-mcp"

var version = "0.2.0"

func main() {
	showVersion := flag.Bool("version", false, "показать версию и выйти")
	dumpConfig := flag.Bool("dump-config", false, "вывести действующие настройки (cfg.json) и выйти")
	dumpTools := flag.Bool("dump-tools", false, "вывести действующие тексты (tools.json) и выйти")
	flag.Parse()
	if *showVersion {
		fmt.Println(serverName, version)
		return
	}

	// stdout занят протоколом MCP — все логи только в stderr.
	log.SetOutput(os.Stderr)
	log.SetFlags(0)
	log.SetPrefix(serverName + ": ")

	exe := exeDir()
	cfg, cfgPath, err := loadConfig(os.Getenv, exe)
	if err != nil {
		log.Fatal(err)
	}
	dirs := []string{exe}
	if cfgPath != "" && filepath.Dir(cfgPath) != exe {
		dirs = []string{filepath.Dir(cfgPath), exe}
	}
	tx, txPath, err := loadTexts(os.Getenv, dirs...)
	if err != nil {
		log.Fatal(err)
	}
	if *dumpConfig || *dumpTools {
		var v any = cfg
		if *dumpTools {
			v = tx
		}
		b, err := marshalPretty(v)
		if err != nil {
			log.Fatal(err)
		}
		os.Stdout.Write(b)
		return
	}
	log.Print("настройки: " + orDefault(cfgPath) + "; тексты: " + orDefault(txPath))

	key := strings.TrimSpace(os.Getenv("NAPARNIK_API_KEY"))
	if key == "" {
		log.Print("NAPARNIK_API_KEY не задан — инструменты будут возвращать ошибку")
	}
	api := naparnik.New(cfg.API.BaseURL, key)
	api.APIPrefix = cfg.API.APIPrefix
	api.MaxSSELine = int(cfg.API.SSEMaxLine)
	api.UserAgent = serverName + "/" + version

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	files, warns := newFileAccess(cfg.FilesRoot, cfg.WriteRoot)
	for _, w := range warns {
		log.Print(w)
	}
	r := newRPC(newServer(api, key != "", files, cfg, tx), os.Stdout)
	if err := r.serve(ctx, os.Stdin); err != nil {
		log.Fatal(err)
	}
}

func orDefault(path string) string {
	if path == "" {
		return "по умолчанию"
	}
	return path
}
