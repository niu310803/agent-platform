package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"

	"agent-platform/internal/webapp"
)

func runWebAppCommand(args []string, out io.Writer) error {
	usage := "usage: agent-platform webapp <check|pack|unpack> [--source <directory-or-zip>] [--output <package.zip>]"
	if len(args) == 0 || args[0] != "check" && args[0] != "pack" && args[0] != "unpack" {
		return fmt.Errorf("%s", usage)
	}
	flags := flag.NewFlagSet("webapp "+args[0], flag.ContinueOnError)
	flags.SetOutput(out)
	source := flags.String("source", "", "application directory, or ZIP for check/unpack (default current directory)")
	output := flags.String("output", "", "ZIP output for pack (default <source>/dist/<key>-<version>.zip), or fresh directory for unpack")
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() > 1 || flags.NArg() == 1 && *source != "" {
		return fmt.Errorf("%s", usage)
	}
	if flags.NArg() == 1 {
		*source = flags.Arg(0)
	}
	if *source == "" {
		*source = "."
	}
	if args[0] == "check" && *output != "" {
		return fmt.Errorf("--output is only valid for pack/unpack")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	var report webapp.PackageReport
	var err error
	if args[0] == "check" {
		report, err = webapp.CheckPackage(ctx, *source)
	} else if args[0] == "unpack" {
		report, err = webapp.UnpackPackage(ctx, *source, *output)
	} else {
		report, err = webapp.PackPackage(ctx, *source, *output)
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(report)
}
