// inventoryd is an observation-only persistence subscriber. It never creates,
// starts, stops, adopts, or deletes runtime containers.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/JH0project/fabric/control/internal/app"
	"github.com/JH0project/fabric/control/internal/inventory"
	"github.com/JH0project/fabric/control/internal/observability"
)

var version = "dev" // supplied by the release image build

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		slog.Error("inventoryd stopped", "error", err)
		os.Exit(1)
	}
}
func run(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("inventoryd", flag.ContinueOnError)
	format := flags.String("log-format", "json", "text or json")
	logLevel := flags.String("log-level", "info", "debug, info, warn, or error")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("inventoryd accepts no positional arguments")
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		return err
	}
	if *format != "json" && *format != "text" {
		return errors.New("log format must be text or json")
	}
	obs, err := observability.New(ctx, observability.Config{ServiceName: "inventoryd", ServiceVersion: version, Level: level, Format: *format})
	if err != nil {
		return err
	}
	obs.Install()
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = obs.Shutdown(c)
	}()
	dsn := os.Getenv("FABRIC_DATABASE_URL")
	if dsn == "" {
		dsn = os.Getenv("FABRIC_INVENTORY_DATABASE_URL")
	}
	store, err := inventory.OpenStore(ctx, dsn)
	if err != nil {
		return err
	}
	defer store.Close()
	bus, err := inventory.BusFromEnv()
	if err != nil {
		return err
	}
	defer bus.Close()
	obs.Logger.Info("inventory persistence subscriber starting")
	p := inventory.Persistence{Bus: bus, Store: store, Logger: obs.Logger}
	return app.New(&p).Run(ctx)
}
