// main.go
//
// gofile is a command line client for gofile.io, inspired by
// github.com/jkawamoto/go-pixeldrain (pd).
package main

import (
	"fmt"
	"os"

	"github.com/urfave/cli/v2"

	"github.com/aldan9610/go-gofile/cmd/gofile/command"
)

// version can be set at build time: -ldflags "-X main.version=v0.1.0".
var version = "dev"

func main() {
	app := &cli.App{
		Name:    "gofile",
		Usage:   "A Gofile client: upload, inspect, list and download files",
		Version: version,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "api-key",
				Aliases: []string{"k"},
				EnvVars: []string{"GOFILE_API_KEY"},
				Usage:   "Gofile API key (account token); guest accounts are used when omitted",
			},
			&cli.StringFlag{
				Name:    "proxy-url",
				EnvVars: []string{"GOFILE_PROXY_URL"},
				Usage: "Route all requests through a URL-prefix proxy, e.g. " +
					"'https://test.cors.workers.dev/?' (useful when gofile is unreachable directly)",
			},
			&cli.BoolFlag{
				Name:    "verbose",
				Aliases: []string{"V"},
				Usage:   "Show detailed information",
			},
		},
		Commands: []*cli.Command{
			command.CmdAccount,
			command.CmdServers,
			command.CmdInfo,
			command.CmdLs,
			command.CmdUpload,
			command.CmdDownload,
			command.CmdRemove,
		},
	}
	if err := app.Run(os.Args); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
