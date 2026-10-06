// Command jannyq is a chat bot that connects messaging platforms to Ollama
// or OpenAI-compatible models, with web search/fetch tools.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/authapon/jannyq/internal/app"
	"github.com/authapon/jannyq/internal/config"
)

// version is set at build time: -ldflags "-X main.version=1.2.3".
var version = "dev"

func main() { os.Exit(run()) }

func run() int {
	for _, a := range os.Args[1:] {
		if a == "--version" || a == "-version" {
			fmt.Println("jannyq", version)
			return 0
		}
	}
	cfg, err := config.Load(os.Args[1:], os.Getenv, os.Stderr)
	if err != nil {
		if errors.Is(err, config.ErrHelp) {
			return 0
		}
		fmt.Fprintln(os.Stderr, "jannyq:", err)
		return 2
	}
	log := app.NewLogger(cfg, os.Stderr)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := app.Run(ctx, cfg, version, log); err != nil {
		log.Error("jannyq stopped", "err", err)
		return 1
	}
	return 0
}
