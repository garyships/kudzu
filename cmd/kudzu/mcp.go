package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	kmcp "github.com/garyships/kudzu/internal/mcp"
)

// cmdMCP serves Kudzu's tools to an AI agent over the Model Context Protocol
// on stdin/stdout. Diagnostics go to stderr; stdout carries only protocol.
func cmdMCP(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	remote := fs.String("remote", "", "remote address (default $KUDZU_REMOTE, else the working copy's remote)")
	readOnly := fs.Bool("read-only", false, "expose only tools that do not write")
	author := fs.String("author", "", "default author for changesets (default $KUDZU_AUTHOR or user@host)")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	rem, err := dialRemote(*remote)
	if err != nil {
		return err
	}
	mode := "read-write"
	if *readOnly {
		mode = "read-only"
	}
	fmt.Fprintf(os.Stderr, "kudzu mcp: serving %s over stdio (%s)\n", rem.Addr, mode)
	srv := kmcp.NewServer(rem, kmcp.Options{ReadOnly: *readOnly, Author: defaultAuthor(*author)})
	if err := srv.Run(ctx, &sdk.StdioTransport{}); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}
