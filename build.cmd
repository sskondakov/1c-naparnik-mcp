@echo off
rem Сборка 1c-naparnik-mcp: проверки, тесты, бинарник в BIN
setlocal
cd /d "%~dp0SRC" || exit /b 1
go vet ./... || exit /b 1
go test ./... || exit /b 1
go build -trimpath -ldflags="-s -w" -o "%~dp0BIN\naparnik-mcp.exe" . || exit /b 1
echo OK: %~dp0BIN\naparnik-mcp.exe
