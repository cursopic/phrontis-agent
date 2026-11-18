// Command phrontis prints what a Kubernetes cluster costs; the code lives in internal/cli so phrontis-api can serve the same report.
package main

import "github.com/cursopic/phrontis/internal/cli"

func main() { cli.Main() }
