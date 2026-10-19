// Command graviton-readiness tells which Kubernetes workloads can move to Graviton (arm64) nodes.
package main

import (
	"os"

	"github.com/robertasolimandonofreo/graviton-readiness/internal/app"
)

var version = "dev"

func main() {
	os.Exit(app.Run(os.Args[1:], app.Env{Stdout: os.Stdout, Stderr: os.Stderr, Version: version}))
}
