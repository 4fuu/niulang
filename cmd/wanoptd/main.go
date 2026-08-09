package main

import (
	"flag"
	"fmt"
	"os"
)

var version = "dev"

func main() {
	mode := flag.String("mode", "", "agent mode: local or server")
	listen := flag.String("listen", "", "local bind address or server bind address")
	remote := flag.String("remote", "", "remote server address for local mode")
	showVersion := flag.Bool("version", false, "print version")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	if *mode != "local" && *mode != "server" {
		fmt.Fprintln(os.Stderr, "wanoptd: --mode must be local or server; implementation is under staged development")
		os.Exit(2)
	}
	if *listen == "" {
		fmt.Fprintln(os.Stderr, "wanoptd: --listen is required")
		os.Exit(2)
	}
	if *mode == "local" && *remote == "" {
		fmt.Fprintln(os.Stderr, "wanoptd: --remote is required in local mode")
		os.Exit(2)
	}
	fmt.Fprintf(os.Stderr, "wanoptd %s: mode=%s listen=%s remote=%s; data plane not enabled yet\n", version, *mode, *listen, *remote)
}
