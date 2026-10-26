@echo off
setlocal
title Klaus Smart
cd /d "%~dp0"
rem ===========================================================================
rem  Startet Qwen 3.8 (llama-server) UND Klaus mit einem Doppelklick.
rem  Diese Datei als start.bat kopieren und den Token eintragen.
rem  start.bat wird von git ignoriert, dein Token landet also nicht auf GitHub.
rem ===========================================================================

set KLAUS_DISCORD_BOT_TOKEN=HIER_DEIN_TOKEN

rem Kanal-IDs fuer "bin da" / "bin weg" (Rechtsklick auf Kanal, Kanal-ID kopieren), mehrere mit Komma
set KLAUS_ANNOUNCE_CHANNELS=

rem --- Modell -----------------------------------------------------------------
set "LLAMA=C:\Users\somos\source\repos\llama.cpp"
rem Altes Modell (zum Zurueckwechseln): Qwen3.8-27B-UD-IQ4_XS.gguf
set "MODEL=%LLAMA%\models\Qwen3.8-27B-GSQ-RCO-IQ3_S-mtp.gguf"
rem Fuer Bilder: mmproj-F16.gguf von Hugging Face laden und unter diesem Namen speichern
set "MMPROJ=%LLAMA%\models\Qwen3.8-27B-mmproj-F16.gguf"
rem Kontext: 49152 (48k) passt mit dem GSQ-Modell in 16 GB, ca. 0,9 GB Luft
set "CTX=49152"
rem Extra-Optionen fuer llama-server, z.B. fuer mehr Tempo: --spec-type draft-mtp
set "LLAMA_EXTRA="
rem Grafik-Schnittstelle: vulkan (Standard) oder rocm (AMD HIP SDK + build-rocm noetig)
set "BACKEND=vulkan"

rem --- Klaus ------------------------------------------------------------------
set KLAUS_API_URL=http://localhost:8080/v1/chat/completions
set KLAUS_CONTEXT_MESSAGES=20
set KLAUS_CONTEXT_GAP_MINUTES=30
set KLAUS_CONTEXT_MAX_AGE_MINUTES=180
set KLAUS_TIMEOUT_MINUTES=30
set KLAUS_THINKING_PREVIEW=1
rem Denk-Stufe: stufen = low (kurz/Smalltalk), medium (normal), xhigh (gross)
rem           auto   = Smalltalk ohne Denken, sonst immer xhigh
rem           oder fest: off, low, medium, xhigh
set KLAUS_REASONING=auto
rem Max. Denk-Tokens pro Antwort, danach antwortet er (0 = unbegrenzt, !denk ignoriert das Limit)
set KLAUS_THINKING_BUDGET=24576
rem Zweite Pruefung: Klaus liest seine Antwort nochmal gegen (1 = an, 0 = aus)
set KLAUS_REVIEW=0
rem Websuche: 1 = an (DuckDuckGo, ohne Anmeldung), 0 = aus
set KLAUS_WEB_SEARCH=1
set KLAUS_MAX_TOOL_ROUNDS=4
rem Zuverlaessigere Suche (1000 Suchen/Monat gratis, tavily.com), sonst leer lassen:
set KLAUS_TAVILY_API_KEY=
rem Eigener SearXNG-Server, sonst leer lassen:
set KLAUS_SEARXNG_URL=

rem --- Mitlesen (antwortet ungefragt, nur wenn er sich sehr sicher ist) ---
rem Kanaele: alle  oder Kanal-IDs mit Komma  oder leer = aus
set KLAUS_PASSIVE_CHANNELS=alle
rem Sekunden warten nach der letzten Nachricht, bevor er prueft
set KLAUS_PASSIVE_WAIT_SECONDS=8
rem Ab wie viel Prozent Sicherheit er antwortet
set KLAUS_PASSIVE_CONFIDENCE=95
rem Nach einer ungefragten Antwort so viele Minuten Pause in dem Kanal
set KLAUS_PASSIVE_COOLDOWN_MINUTES=10
rem Testmodus: 1 = schreibt nichts, zeigt nur im Fenster, was er gesagt haette
set KLAUS_PASSIVE_TESTMODE=0

rem ===========================================================================

rem Modell einmalig aus .ollama\kis zu den anderen Modellen holen (gleiches Laufwerk = sofort)
if not exist "%MODEL%" if exist "C:\Users\somos\.ollama\kis\Qwen3.8-27B-GSQ-RCO-IQ3_S-mtp.gguf" (
    echo Verschiebe Qwen3.8 nach llama.cpp\models ...
    move "C:\Users\somos\.ollama\kis\Qwen3.8-27B-GSQ-RCO-IQ3_S-mtp.gguf" "%MODEL%" >nul
)
if not exist "%MODEL%" (
    echo FEHLER: Modell nicht gefunden: %MODEL%
    pause
    exit /b 1
)

rem Laeuft schon ein llama-server auf Port 8080?
curl -s -f http://127.0.0.1:8080/health >nul 2>&1
if not errorlevel 1 goto server_running

set "VISION=--no-mmproj"
if exist "%MMPROJ%" (
    rem Bild-Teil auf der CPU, damit die 16 GB VRAM fuer Modell und Kontext bleiben
    set VISION=--mmproj "%MMPROJ%" --no-mmproj-offload --image-max-tokens 1024
    echo Bilder: an
) else (
    echo Bilder: aus - Datei fehlt: %MMPROJ%
)

set "SERVER_EXE=%LLAMA%\build\bin\Release\llama-server.exe"
if /i "%BACKEND%"=="rocm" (
    set "SERVER_EXE=%LLAMA%\build-rocm\bin\llama-server.exe"
    set "PATH=%HIP_PATH%bin;%HIP_PATH%\bin;%PATH%"
    rem Nur die 9070 XT nutzen (ROCm0 waere die Prozessor-Grafik)
    set "HIP_VISIBLE_DEVICES=1"
)
if not exist "%SERVER_EXE%" (
    echo FEHLER: %SERVER_EXE% fehlt. Fuer rocm erst build-rocm bauen, sonst BACKEND=vulkan setzen.
    pause
    exit /b 1
)
echo Starte llama-server (%BACKEND%) im eigenen Fenster ...
set GGML_VK_DISABLE_HOST_VISIBLE_VIDMEM=1
start "Qwen 3.8 Server" /D "%LLAMA%" cmd /k ""%SERVER_EXE%" -m "%MODEL%" --host 127.0.0.1 --port 8080 -ngl 99 -c %CTX% -fa on -ctk q8_0 -ctv q8_0 -np 1 -b 256 -ub 128 --load-mode none --jinja %VISION% %LLAMA_EXTRA%"

echo Warte, bis das Modell geladen ist ...
set /a WAITED=0
:wait_loop
timeout /t 2 /nobreak >nul
curl -s -f http://127.0.0.1:8080/health >nul 2>&1
if not errorlevel 1 goto server_ready
set /a WAITED+=2
if %WAITED% GEQ 300 (
    echo llama-server ist nach 5 Minuten noch nicht bereit. Schau ins Server-Fenster.
    pause
    exit /b 1
)
goto wait_loop

:server_running
curl -s http://127.0.0.1:8080/props | findstr /i "Qwen3.8" >nul
if errorlevel 1 (
    echo ACHTUNG: Auf Port 8080 laeuft schon ein ANDERES Modell. Klaus nutzt jetzt das.
    echo Schliess das andere Server-Fenster, wenn du Qwen 3.8 willst.
    pause
) else (
    echo llama-server mit Qwen 3.8 laeuft schon.
)

:server_ready
echo Modell bereit.
where go >nul 2>&1
if errorlevel 1 (
    echo FEHLER: Go wurde nicht gefunden, Klaus kann nicht gebaut werden.
    echo Installiere Go von https://go.dev/dl/ und starte diese Datei neu.
    pause
    exit /b 1
)
echo Lade Abhaengigkeiten ...
go mod tidy
if errorlevel 1 (
    echo go mod tidy fehlgeschlagen - Internet?
    pause
    exit /b 1
)
echo Baue Klaus ...
go build -o klaus-smart.exe .
if errorlevel 1 (
    echo Build fehlgeschlagen - siehe Fehler oben.
    pause
    exit /b 1
)

rem yt-dlp fuer YouTube-Videos (einmal laden, danach selbst aktualisieren)
if not exist yt-dlp.exe (
    echo Lade yt-dlp fuer YouTube ...
    curl -L -s -f -o yt-dlp.exe https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp.exe
) else (
    yt-dlp.exe -U >nul 2>&1
)

klaus-smart.exe

echo.
choice /m "llama-server auch beenden"
if errorlevel 2 goto end
taskkill /fi "WINDOWTITLE eq Qwen 3.8 Server*" /t /f >nul 2>&1
:end
endlocal
