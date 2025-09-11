#!/bin/bash

echo "Build for Linux "
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o ./bin/dm

echo "Build for Windows "
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o ./bin/dm.exe

echo "Build completed"
