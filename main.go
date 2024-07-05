package main

import (
	"log/slog"
	"os"

	"github.com/be9/tbc/cmd"
)

func main() {
	app := cmd.CreateApp()

	if err := app.Run(os.Args); err != nil {
		slog.Error(err.Error())
	}
}
