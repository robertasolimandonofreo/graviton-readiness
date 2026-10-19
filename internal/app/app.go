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

	"github.com/robertasolimandonofreo/graviton-readiness/internal/cluster"
	"github.com/robertasolimandonofreo/graviton-readiness/internal/image"
	"github.com/robertasolimandonofreo/graviton-readiness/internal/inventory"
	"github.com/robertasolimandonofreo/graviton-readiness/internal/plan"
	"github.com/robertasolimandonofreo/graviton-readiness/internal/report"
	"github.com/robertasolimandonofreo/graviton-readiness/internal/rules"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/dynamic"
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
	// Cluster replaces the client of the live-cluster mode. Tests use it.
	Cluster dynamic.Interface
}

const usage = `graviton-readiness tells which Kubernetes workloads can move to Graviton (arm64) nodes.

Usage:
  graviton-readiness scan [flags] [paths...]   scan manifests (files, directories, or - for stdin)
  graviton-readiness scan --cluster [flags]    read the workloads, NodePools and nodes from a live cluster
  graviton-readiness plan [flags] [paths...]   what to move now, after changes, or never
  graviton-readiness explain <rule>            describe a rule, for example GR004
  graviton-readiness version

Live cluster (read-only: it only lists, with paging and a low request rate):
      --cluster                read the Deployments, StatefulSets, DaemonSets, CronJobs, Argo Rollouts,
                               Karpenter NodePools and EC2NodeClasses, and the nodes (their labels tell
                               the architecture of pools that are not Karpenter's)
      --kubeconfig <file>      kubeconfig to use (default: the usual one)
      --context <name>         kubeconfig context to use (default: the current one)
  -n, --namespace <name>       with --cluster, read the workloads of this namespace only

Scan flags:
  -f, --format <table|json|markdown|sarif>
                                       output format (default table); sarif is for GitHub Code Scanning
      --check-images                   read the platforms of every image from its registry
      --fail-on <not-ready|unverified|caveats>
                                       exit 1 when a workload reaches this level
  -v, --verbose                        print every finding
      --json-file <file>               also write the JSON report to this file
      --sarif-file <file>              also write the SARIF report to this file
      --plan-file <file>               also write the migration plan, in Markdown, to this file
                                       (the three come from one run: the registries are asked once)
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
	case "plan":
		return planCommand(args[1:], env)
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
	cluster     bool
	kubeconfig  string
	kubeContext string
	namespace   string
	jsonFile    string
	sarifFile   string
	planFile    string
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

// session is everything that scan and plan share: the flags, what was read and the result of
// the rules for every workload.
type session struct {
	flags       scanFlags
	paths       []string
	inv         *inventory.Inventory
	assessments []rules.Assessment
	ro          report.Options
}

// prepare parses the flags, reads the manifests, checks the images when asked and applies the
// rules. It returns a nil session and an exit code when something stops the command.
func prepare(command string, formats []string, args []string, env Env) (*session, int) {
	var f scanFlags
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
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
	fs.StringVar(&f.jsonFile, "json-file", "", "")
	fs.StringVar(&f.sarifFile, "sarif-file", "", "")
	fs.StringVar(&f.planFile, "plan-file", "", "")
	fs.BoolVar(&f.cluster, "cluster", false, "")
	fs.StringVar(&f.kubeconfig, "kubeconfig", "", "")
	fs.StringVar(&f.kubeContext, "context", "", "")
	fs.StringVar(&f.namespace, "namespace", "", "")
	fs.StringVar(&f.namespace, "n", "", "")
	paths, err := parseInterspersed(fs, args)
	if err != nil {
		return nil, exitUsage
	}
	fail := func(format string, a ...any) (*session, int) {
		fmt.Fprintf(env.Stderr, format+"\n", a...)
		return nil, exitUsage
	}
	if len(paths) == 0 && !f.cluster {
		return fail("%s needs at least one path (a file, a directory, or - for stdin), or --cluster", command)
	}
	known := false
	for _, name := range formats {
		known = known || f.format == name
	}
	if !known {
		return fail("unknown format %q (use %s)", f.format, strings.Join(formats, ", "))
	}
	switch f.failOn {
	case "", "not-ready", "unverified", "caveats":
	default:
		return fail("unknown --fail-on %q (use not-ready, unverified or caveats)", f.failOn)
	}
	if command != "scan" && f.failOn != "" {
		return fail("--fail-on belongs to scan: %s only plans", command)
	}
	if command != "scan" && (f.jsonFile != "" || f.sarifFile != "" || f.planFile != "") {
		return fail("--json-file, --sarif-file and --plan-file belong to scan")
	}

	inv, err := inventory.Paths(paths, env.Stdin)
	if err != nil {
		return fail("error: %v", err)
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if f.cluster {
		fmt.Fprintln(env.Stderr, "reading the cluster (read-only: list calls only)...")
		res, err := cluster.Read(ctx, inv, cluster.Options{Kubeconfig: f.kubeconfig, Context: f.kubeContext,
			Namespace: f.namespace, Client: env.Cluster, Notes: env.Stderr})
		if err != nil {
			return fail("error: %v", err)
		}
		fmt.Fprintf(env.Stderr, "read cluster %s: %d Deployments, %d StatefulSets, %d DaemonSets, %d CronJobs, %d Rollouts, %d NodePools, %d EC2NodeClasses, %d nodes\n",
			res.Context, res.Counts["deployments"], res.Counts["statefulsets"], res.Counts["daemonsets"], res.Counts["cronjobs"], res.Counts["rollouts"],
			res.Counts["nodepools"], res.Counts["ec2nodeclasses"], res.Counts["nodes"])
	}
	inv.Sort()
	if len(inv.Workloads) == 0 {
		// Not an error, but a typo in the path would otherwise pass a CI check silently.
		where := "the cluster"
		if len(paths) > 0 {
			where = strings.Join(paths, ", ")
		}
		fmt.Fprintf(env.Stderr, "warning: no workloads found in %s (Deployments, StatefulSets, DaemonSets, Rollouts, Jobs, CronJobs and Pods are read)\n", where)
	}
	var results map[string]image.Result
	if f.checkImages {
		checker := env.Checker
		if checker == nil {
			d, err := parseTimeout(f.timeout)
			if err != nil {
				return fail("error: %v", err)
			}
			checker = &image.Registry{Timeout: d}
		}
		results = image.CheckAll(ctx, checker, rules.ImageRefs(inv.Workloads), f.concurrency)
	}

	inv.LinkNodeClasses()
	pools := inv.NodePools
	for _, spec := range f.nodePools {
		pool, err := rules.ParsePool(spec)
		if err != nil {
			return fail("error: %v", err)
		}
		pools = append(pools, pool)
	}
	// Pools that are not Karpenter's take the architecture of their nodes, when a cluster was read.
	var pods []corev1.PodSpec
	for _, w := range inv.Workloads {
		pods = append(pods, w.Pod)
	}
	pools = append(pools, rules.ObservedPools(pods, pools, inv.Nodes)...)
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
	return &session{flags: f, paths: paths, inv: inv, assessments: assessments,
		ro: report.Options{Verbose: f.verbose, ImagesChecked: f.checkImages, Version: env.Version, Pools: info}}, exitOK
}

func scan(args []string, env Env) int {
	s, code := prepare("scan", []string{"table", "json", "markdown", "md", "sarif"}, args, env)
	if s == nil {
		return code
	}
	var werr error
	switch s.flags.format {
	case "json":
		werr = report.JSON(env.Stdout, s.assessments, s.inv.Skipped, s.ro)
	case "sarif":
		werr = report.SARIF(env.Stdout, s.assessments, s.ro)
	case "markdown", "md":
		werr = report.Markdown(env.Stdout, s.assessments, s.inv.Skipped, s.ro)
	default:
		werr = report.Table(env.Stdout, s.assessments, s.inv.Skipped, s.ro)
	}
	if werr != nil {
		fmt.Fprintln(env.Stderr, "error:", werr)
		return exitUsage
	}
	// The other reports come from the same run, so the registries are not asked again.
	for _, out := range []struct {
		path  string
		write func(io.Writer) error
	}{
		{s.flags.jsonFile, func(w io.Writer) error { return report.JSON(w, s.assessments, s.inv.Skipped, s.ro) }},
		{s.flags.sarifFile, func(w io.Writer) error { return report.SARIF(w, s.assessments, s.ro) }},
		{s.flags.planFile, func(w io.Writer) error { return report.PlanMarkdown(w, plan.Build(s.assessments), s.ro) }},
	} {
		if out.path == "" {
			continue
		}
		if err := writeFile(out.path, out.write); err != nil {
			fmt.Fprintf(env.Stderr, "error: writing %s: %v\n", out.path, err)
			return exitUsage
		}
	}
	if reached(s.assessments, s.flags.failOn) {
		return exitFail
	}
	return exitOK
}

// writeFile creates path and fills it with write, reporting the error of the close too.
func writeFile(path string, write func(io.Writer) error) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	err = write(f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
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

// planCommand turns the verdicts into an order of work.
func planCommand(args []string, env Env) int {
	s, code := prepare("plan", []string{"table", "json", "markdown", "md"}, args, env)
	if s == nil {
		return code
	}
	p := plan.Build(s.assessments)
	var werr error
	switch s.flags.format {
	case "json":
		werr = report.PlanJSON(env.Stdout, p, s.ro)
	case "markdown", "md":
		werr = report.PlanMarkdown(env.Stdout, p, s.ro)
	default:
		werr = report.PlanTable(env.Stdout, p, s.ro)
	}
	if werr != nil {
		fmt.Fprintln(env.Stderr, "error:", werr)
		return exitUsage
	}
	return exitOK
}
