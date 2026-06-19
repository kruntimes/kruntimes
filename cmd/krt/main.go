package main

import (
	"os"

	"github.com/kruntimes/kruntimes/internal/krt"
)

func main() {
	if err := krt.NewRootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}
