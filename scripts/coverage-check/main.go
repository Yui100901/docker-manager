package main

import (
	"os"

	"docker-manager/internal/coveragegate"
)

func main() {
	os.Exit(coveragegate.Run(os.Args[1:], os.Stdout, os.Stderr))
}
