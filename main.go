package main

import (
	"os"

	"docker-manager/internal/cli"
)

func main() {
	os.Exit(cli.Run())
}
