// 1c-naparnik-mcp — MCP-сервер (stdio) для публичного API 1С:Напарника.
//
// У каждой настройки одно место. Переменные окружения — настройки подключения проекта:
//
//	NAPARNIK_API_KEY    — API-ключ из раздела «Ключи доступа» (обязательно)
//	NAPARNIK_FILES_ROOT — каталоги, которые 1С:Напарник может читать, через ';' (необязательно)
//	NAPARNIK_WRITE_ROOT — каталоги, в которых 1С:Напарник может создавать и править файлы, через ';';
//	                      каждый должен быть внутри одного из каталогов чтения (необязательно)
//	NAPARNIK_LOG        — путь к файлу журнала; не задана — журнала нет
//	NAPARNIK_LOG_LEVEL  — error, info (по умолчанию) или debug
//	NAPARNIK_CONFIG     — путь к cfg.json (по умолчанию cfg.json рядом с exe, если есть)
//	NAPARNIK_TOOLS      — путь к tools.json (по умолчанию tools.json рядом с cfg.json или exe, если есть)
//
// cfg.json — настройки установки (адрес API, таймауты, лимиты), tools.json — тексты для агента и 1С:Напарника.
// В обоих файлах указываются только изменяемые ключи; полный шаблон: -dump-config, -dump-tools.
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

var version = "0.3.0"

func main() {
	showVersion := flag.Bool("version", false, "показать версию и выйти")
	dumpConfig := flag.Bool("dump-config", false, "вывести действующие настройки (cfg.json) и выйти")
	dumpTools := flag.Bool("dump-tools", false, "вывести действующие тексты (tools.json) и выйти")
	flag.Parse()
	if *showVersion {
		fmt.Println(serverName, version)
		return
	}

	// stdout занят протоколом MCP — все сообщения только в stderr (и в журнал, если он включён).
	log.SetOutput(os.Stderr)
	log.SetFlags(0)
	log.SetPrefix(serverName + ": ")

	level, err := parseLogLevel(os.Getenv("NAPARNIK_LOG_LEVEL"))
	if err != nil {
		log.Fatal(err)
	}
	j, warn := openJournal(os.Getenv("NAPARNIK_LOG"), level)
	if warn != "" {
		log.Print(warn)
	}
	defer j.close()
	fatal := func(err error) {
		j.errorf("запуск: %v", err)
		j.close()
		log.Fatal(err)
	}

	exe := exeDir()
	cfg, cfgPath, err := loadConfig(os.Getenv, exe)
	if err != nil {
		fatal(err)
	}
	dirs := []string{exe}
	if cfgPath != "" && filepath.Dir(cfgPath) != exe {
		dirs = []string{filepath.Dir(cfgPath), exe}
	}
	tx, txPath, err := loadTexts(os.Getenv, dirs...)
	if err != nil {
		fatal(err)
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
	j.infof("запуск %s %s; настройки: %s; тексты: %s", serverName, version, orDefault(cfgPath), orDefault(txPath))

	key := strings.TrimSpace(os.Getenv("NAPARNIK_API_KEY"))
	if key == "" {
		log.Print("NAPARNIK_API_KEY не задан — инструменты будут возвращать ошибку")
		j.errorf("NAPARNIK_API_KEY не задан")
	}
	api := naparnik.New(cfg.API.BaseURL, key)
	api.APIPrefix = cfg.API.APIPrefix
	api.MaxSSELine = int(cfg.API.SSEMaxLine)
	api.UserAgent = serverName + "/" + version
	if j != nil {
		api.OnRequest = j.apiRequest
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	files, warns := newFileAccess(envList("NAPARNIK_FILES_ROOT"), envList("NAPARNIK_WRITE_ROOT"))
	for _, w := range warns {
		log.Print(w)
		j.infof("%s", w)
	}
	j.infof("каталоги чтения: %s; записи: %s", orDash(joinSlash(files.read)), orDash(joinSlash(files.write)))
	r := newRPC(newServer(api, key != "", files, cfg, tx, j), os.Stdout)
	if err := r.serve(ctx, os.Stdin); err != nil {
		fatal(err)
	}
	j.infof("остановка")
}

// envList — список путей из переменной окружения (разделитель ';' на Windows).
func envList(name string) []string {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return nil
	}
	return filepath.SplitList(v)
}

func orDefault(path string) string {
	if path == "" {
		return "по умолчанию"
	}
	return path
}
