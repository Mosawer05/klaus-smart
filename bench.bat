@echo off
title Klaus Denk-Stufen-Test
cd /d "%~dp0"
curl -s -f http://127.0.0.1:8080/health >nul 2>&1
if errorlevel 1 (
    echo Der llama-server laeuft nicht. Starte zuerst start.bat und warte, bis Qwen geladen ist.
    pause
    exit /b 1
)
echo Test laeuft etwa 15-25 Minuten. Bitte Klaus in der Zeit nicht anpingen.
echo Ergebnis landet in bench-ergebnis.txt
echo.
go run ./bench
pause
