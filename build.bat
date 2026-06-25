@echo off
setlocal
if "%VERSION%"=="" set VERSION=dev
if "%COMMIT%"=="" for /f %%i in ('git rev-parse --short HEAD 2^>nul') do set COMMIT=%%i
if "%COMMIT%"=="" set COMMIT=unknown
if "%BUILD_DATE%"=="" for /f %%i in ('powershell -NoProfile -Command "Get-Date -AsUTC -Format yyyy-MM-ddTHH:mm:ssZ"') do set BUILD_DATE=%%i
set LDFLAGS=-s -w -X docker-manager/internal/version.version=%VERSION% -X docker-manager/internal/version.commit=%COMMIT% -X docker-manager/internal/version.buildDate=%BUILD_DATE%

echo Build for Linux ...
set GOOS=linux
set GOARCH=amd64
set CGO_ENABLED=0
go build -ldflags "%LDFLAGS%" -o ./bin/linux/dm .

echo Build for Windows ...
set GOOS=windows
set GOARCH=amd64
set CGO_ENABLED=0
go build -ldflags "%LDFLAGS%" -o ./bin/windows/dm.exe .

echo Build completed
endlocal
