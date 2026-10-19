// Package app wires the command line to the loader, the image checker, the rules and the reports.
package app

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	"github.com/robertasolimandonofreo/graviton-readiness/internal/image"
	"github.com/robertasolimandonofreo/graviton-readiness/internal/inventory"
	"github.com/robertasolimandonofreo/graviton-readiness/internal/report"
	"github.com/robertasolimandonofreo/graviton-readiness/internal/rules"
)

// Exit codes: 0 success, 1 the --fail-on threshold was reached, 2 a usage or runtime error.
const (
	exitOK    = 0
	exitFail  = 1
	exitUsage = 2
)

// Env carries what the command needs from its surroundings, so tests can replace it.
type Env struct {
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer
	Version string
	// Checker overrides the registry client. Tests use it; nil means the real registries.
	Checker image.Checker
}

const usage = `graviton-readiness tells which Kubernetes workloads can move to Graviton (arm64) nodes.

Usage:
  graviton-readiness scan [flags] [paths...]   scan manifests (files, directories, or - for stdin)
  graviton-readiness explain <rule>            describe a rule, for example GR004
  graviton-readiness version

Scan flags:
  -f, --format <table|json|markdown>   output format (default table)
      --check-images                   read the platforms of every image from its registry
      --fail-on <not-ready|unverified|caveats>
                                       exit 1 when a workload reaches this level
  -v, --verbose                        print every finding
      --timeout <duration>             limit for one registry lookup (default 20s)
      --concurrency <n>                registry lookups at a time (default 8)
      --node-pool <key=value:arch>     the architecture (amd64, arm64 or any) of nodes labeled key=value, for
                                       pools that are not Karpenter NodePools (EKS node groups, GKE or AKS
                                       pools); may be repeated
`

// Run executes one command line and returns the process exit code.
func Run(args []string, env Env) int {
	if env.Stdin == nil {
		env.Stdin = os.Stdin
	}
	if len(args) == 0 {
		fmt.Fprint(env.Stderr, usage)
		return exitUsage
	}
	switch args[0] {
	case "scan":
		return scan(args[1:], env)
	case "explain":
		return explain(args[1:], env)
	case "version", "--version":
		fmt.Fprintln(env.Stdout, "graviton-readiness", env.Version)
		return exitOK
	case "help", "-h", "--help":
		fmt.Fprint(env.Stdout, usage)
		return exitOK
	}
	fmt.Fprintf(env.Stderr, "unknown command %q\n\n%s", args[0], usage)
	return exitUsage
}

type scanFlags struct {
	format      string
	checkImages bool
	failOn      string
	verbose     bool
	timeout     string
	concurrency int
	nodePools   poolFlags
}

// poolFlags collects every --node-pool, which may be repeated.
type poolFlags []string

func (p *poolFlags) String() string     { return strings.Join(*p, ",") }
func (p *poolFlags) Set(v string) error { *p = append(*p, v); return nil }

// parseInterspersed parses flags that may come before or after the paths, which the standard
// flag package does not do by itself: it stops at the first non-flag argument.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var rest []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return rest, nil
		}
		rest = append(rest, args[0])
		args = args[1:]
	}
}

func scan(args []string, env Env) int {
	var f scanFlags
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	fs.StringVar(&f.format, "format", "table", "")
	fs.StringVar(&f.format, "f", "table", "")
	fs.BoolVar(&f.checkImages, "check-images", false, "")
	fs.StringVar(&f.failOn, "fail-on", "", "")
	fs.BoolVar(&f.verbose, "verbose", false, "")
	fs.BoolVar(&f.verbose, "v", false, "")
	fs.StringVar(&f.timeout, "timeout", "20s", "")
	fs.IntVar(&f.concurrency, "concurrency", 8, "")
	fs.Var(&f.nodePools, "node-pool", "")
	paths, err := parseInterspersed(fs, args)
	if err != nil {
		return exitUsage
	}
	if len(paths) == 0 {
		fmt.Fprintln(env.Stderr, "scan needs at least one path (a file, a directory, or - for stdin)")
		return exitUsage
	}
	switch f.format {
	case "table", "json", "markdown", "md":
	default:
		fmt.Fprintf(env.Stderr, "unknown format %q (use table, json or markdown)\n", f.format)
		return exitUsage
	}
	switch f.failOn {
	case "", "not-ready", "unverified", "caveats":
	default:
		fmt.Fprintf(env.Stderr, "unknown --fail-on %q (use not-ready, unverified or caveats)\n", f.failOn)
		return exitUsage
	}

	inv, err := inventory.Paths(paths, env.Stdin)
	if err != nil {
		fmt.Fprintln(env.Stderr, "error:", err)
		return exitUsage
	}
	if n := len(inv.Unreadable); n > 0 {
		shown := inv.Unreadable
		if len(shown) > 3 {
			shown = shown[:3]
		}
		fmt.Fprintf(env.Stderr, "warning: skipped %d files that are not valid Kubernetes YAML (Helm templates, for example); first: ", n)
		for i, u := range shown {
			if i > 0 {
				fmt.Fprint(env.Stderr, ", ")
			}
			fmt.Fprint(env.Stderr, u.Path)
		}
		fmt.Fprintln(env.Stderr)
	}
	if len(inv.Workloads) == 0 {
		// Not an error, but a typo in the path would otherwise pass a CI check silently.
		fmt.Fprintf(env.Stderr, "warning: no workloads found in %s (Deployments, StatefulSets, DaemonSets, Jobs, CronJobs and Pods are read)\n", strings.Join(paths, ", "))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var results map[string]image.Result
	if f.checkImages {
		checker := env.Checker
		if checker == nil {
			d, err := parseTimeout(f.timeout)
			if err != nil {
				fmt.Fprintln(env.Stderr, "error:", err)
				return exitUsage
			}
			checker = &image.Registry{Timeout: d}
		}
		results = image.CheckAll(ctx, checker, rules.ImageRefs(inv.Workloads), f.concurrency)
	}

	pools := inv.NodePools
	for _, spec := range f.nodePools {
		pool, err := rules.ParsePool(spec)
		if err != nil {
			fmt.Fprintln(env.Stderr, "error:", err)
			return exitUsage
		}
		pools = append(pools, pool)
	}
	opts := rules.Options{ImagesChecked: f.checkImages, NodePools: pools}
	assessments := make([]rules.Assessment, 0, len(inv.Workloads))
	for _, w := range inv.Workloads {
		assessments = append(assessments, rules.Evaluate(w, results, opts))
	}

	info := report.PoolInfo{}
	info.Read, info.ARM64 = rules.PoolCounts(pools)
	for _, w := range inv.Workloads {
		if rules.PoolUnknown(w.Pod, pools) {
			info.Unresolved++
		}
	}
	ro := report.Options{Verbose: f.verbose, ImagesChecked: f.checkImages, Version: env.Version, Pools: info}
	var werr error
	switch f.format {
	case "json":
		werr = report.JSON(env.Stdout, assessments, inv.Skipped, ro)
	case "markdown", "md":
		werr = report.Markdown(env.Stdout, assessments, inv.Skipped, ro)
	default:
		werr = report.Table(env.Stdout, assessments, inv.Skipped, ro)
	}
	if werr != nil {
		fmt.Fprintln(env.Stderr, "error:", werr)
		return exitUsage
	}
	if reached(assessments, f.failOn) {
		return exitFail
	}
	return exitOK
}

// reached tells whether any workload is at or beyond the --fail-on level. The levels are
// cumulative: caveats also fails on unverified and not-ready.
func reached(as []rules.Assessment, level string) bool {
	for _, a := range as {
		switch a.Verdict {
		case rules.NotReady:
			if level != "" {
				return true
			}
		case rules.Unverified:
			if level == "unverified" || level == "caveats" {
				return true
			}
		case rules.ReadyWithCaveats:
			if level == "caveats" {
				return true
			}
		}
	}
	return false
}

func explain(args []string, env Env) int {
	if len(args) != 1 {
		fmt.Fprintln(env.Stderr, "usage: graviton-readiness explain <rule>, for example GR004")
		return exitUsage
	}
	id := strings.ToUpper(args[0])
	r, ok := rules.Lookup(id)
	if !ok {
		var ids []string
		for _, r := range rules.Catalog {
			ids = append(ids, r.ID)
		}
		fmt.Fprintf(env.Stderr, "unknown rule %q (rules: %s)\n", args[0], strings.Join(ids, ", "))
		return exitUsage
	}
	fmt.Fprintf(env.Stdout, "%s [%s] %s\n\nWhy it matters\n  %s\n\nHow to fix it\n  %s\n", r.ID, r.Severity, r.Title, r.Why, r.Fix)
	if r.Example != "" {
		fmt.Fprintf(env.Stdout, "\nExample\n%s\n", indentLines(r.Example, "  "))
	}
	return exitOK
}

func indentLines(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}
