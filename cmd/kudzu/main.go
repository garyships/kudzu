// Command kudzu is an open-source, agent-scale source control system modeled
// on the design Anthropic described for Scion: changesets in a log, derived
// data, a server that does the heavy lifting, want-based object fetching
// through tiered caches, and thin sparse clients.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// version is set at release time with -ldflags "-X main.version=...".
var version = "dev"

func cmdVersion(context.Context, []string) error {
	fmt.Println("kudzu", version)
	return nil
}

type command struct {
	name    string
	summary string
	run     func(ctx context.Context, args []string) error
}

var commands []command

func init() {
	commands = []command{
		{"up", "one command: import this git repo and run all three tiers", cmdUp},
		{"version", "print the kudzu version", cmdVersion},
		{"server", "run the authoritative server", cmdServer},
		{"cache", "run a regional object cache (in memory)", cmdCache},
		{"daemon", "run a per-node object daemon (on disk, unix socket)", cmdDaemon},
		{"import", "import git history into a server store", cmdImport},
		{"export", "export a ref's history from a server store to a git repository", cmdExport},
		{"snapshot", "save a revision's objects for seeding a daemon", cmdSnapshot},
		{"token", "generate an access token for a server's --auth policy", cmdToken},

		{"clone", "create a (sparse) working copy", cmdClone},
		{"status", "show local changes", cmdStatus},
		{"commit", "create a changeset on the server from local changes", cmdCommit},
		{"update", "move the working copy to another revision", cmdUpdate},
		{"rebase", "rebase the working copy's stack on the server", cmdRebase},
		{"land", "land the working copy's stack onto a ref (lock-free)", cmdLand},
		{"sparse", "show or change the sparse checkout", cmdSparse},
		{"gc", "trim the local object cache, or collect a server store (--store)", cmdGC},

		{"log", "show history (of the repo, a file or a directory)", cmdLog},
		{"blame", "show who last changed each line", cmdBlame},
		{"diff", "show changes (local, or between revisions)", cmdDiff},
		{"show", "show a changeset", cmdShow},
		{"cat", "print a file at a revision", cmdCat},
		{"ls", "list a directory at a revision", cmdLs},
		{"refs", "list refs", cmdRefs},
		{"merge-base", "find the merge base of two revisions", cmdMergeBase},
		{"write", "commit files through the API, without a checkout", cmdWrite},
		{"stats", "show a tier's cache statistics", cmdStats},

		{"mcp", "serve the tools to AI agents over MCP (stdio)", cmdMCP},
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, "usage: kudzu <command> [flags] [args]\n\ncommands:\n")
	for _, c := range commands {
		fmt.Fprintf(os.Stderr, "  %-11s %s\n", c.name, c.summary)
	}
	fmt.Fprintf(os.Stderr, "\nRun 'kudzu <command> -h' for a command's flags.\n")
}

func main() {
	if len(os.Args) == 2 && (os.Args[1] == "--version" || os.Args[1] == "-v") {
		fmt.Println("kudzu", version)
		return
	}
	if len(os.Args) < 2 || os.Args[1] == "-h" || os.Args[1] == "--help" || os.Args[1] == "help" {
		usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	for _, c := range commands {
		if c.name == os.Args[1] {
			if err := c.run(ctx, os.Args[2:]); err != nil {
				if errors.Is(err, flag.ErrHelp) {
					os.Exit(2)
				}
				if jsonOutput {
					printJSONError(err)
					os.Exit(1)
				}
				fmt.Fprintf(os.Stderr, "kudzu %s: %v\n", c.name, err)
				os.Exit(1)
			}
			return
		}
	}
	fmt.Fprintf(os.Stderr, "kudzu: unknown command %q\n\n", os.Args[1])
	usage()
	os.Exit(2)
}

// parse lets flags and positional arguments appear in any order.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		if rest[0] == "--" {
			return append(pos, rest[1:]...), nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

// parseDuration accepts time.ParseDuration's units plus d (days) and w
// (weeks), e.g. 30d or 1w2d.
func parseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty duration")
	}
	var total time.Duration
	for len(s) > 0 {
		i := strings.IndexAny(s, "dw")
		if i < 0 {
			d, err := time.ParseDuration(s)
			if err != nil || d < 0 {
				return 0, fmt.Errorf("bad duration %q", s)
			}
			return total + d, nil
		}
		n, err := strconv.ParseFloat(s[:i], 64)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("bad duration %q", s)
		}
		unit := 24 * time.Hour
		if s[i] == 'w' {
			unit *= 7
		}
		total += time.Duration(n * float64(unit))
		s = s[i+1:]
	}
	return total, nil
}

func parseSize(s string) (int64, error) {
	s = strings.ToUpper(strings.TrimSpace(s))
	mult := int64(1)
	for _, u := range []struct {
		suffix string
		mult   int64
	}{{"TB", 1 << 40}, {"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10}, {"B", 1}} {
		if strings.HasSuffix(s, u.suffix) {
			s, mult = strings.TrimSuffix(s, u.suffix), u.mult
			break
		}
	}
	var n float64
	if _, err := fmt.Sscanf(s, "%g", &n); err != nil {
		return 0, fmt.Errorf("bad size %q", s)
	}
	return int64(n * float64(mult)), nil
}
