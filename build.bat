@echo off
echo Build for Linux ...
set GOOS=linux
set GOARCH=amd64
set CGO_ENABLED=0
go build -o ./bin/linux/dm

echo Build for Windows ...
set GOOS=windows
set GOARCH=amd64
set CGO_ENABLED=0
go build -o ./bin/windows/dm.exe

echo Build completed
pause
