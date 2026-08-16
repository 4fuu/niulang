package main

import "testing"

// A default is not an instruction.
//
// The mode checks reject flags belonging to the other mode, and they used to
// reject them by looking at the value rather than at whether the operator had
// given it. That works only while every such flag defaults to its zero value,
// and it failed the moment one did not: --quic-pool became the default, and
// every server refused to start because a default it had never been given
// looked exactly like one it had.
func TestADefaultIsNotAFlagTheOperatorGave(t *testing.T) {
	server := []string{
		"--mode", "server", "--listen", ":443",
		"--secret-file", "/dev/null", "--tls-cert", "/dev/null", "--tls-key", "/dev/null",
	}
	if _, err := parseOptions(server); err != nil {
		t.Fatalf("a server with no local flags was refused: %v", err)
	}
	if _, err := parseOptions(append(append([]string(nil), server...), "--quic-pool")); err == nil {
		t.Fatal("a server given a local-only flag was accepted")
	}

	local := []string{
		"--mode", "local", "--listen", "127.0.0.1:1080",
		"--remote", "peer:443", "--server-name", "peer", "--secret-file", "/dev/null",
	}
	if _, err := parseOptions(local); err != nil {
		t.Fatalf("a local agent with no server flags was refused: %v", err)
	}
	if _, err := parseOptions(append(append([]string(nil), local...), "--allow-private-destinations")); err == nil {
		t.Fatal("a local agent given a server-only flag was accepted")
	}
}

// The pool is what makes opening a flow cost nothing, so it is on unless the
// operator turns it off.
func TestTheConnectionPoolIsOnUnlessRefused(t *testing.T) {
	local := []string{
		"--mode", "local", "--listen", "127.0.0.1:1080",
		"--remote", "peer:443", "--server-name", "peer", "--secret-file", "/dev/null",
	}
	opts, err := parseOptions(local)
	if err != nil {
		t.Fatal(err)
	}
	if !opts.quicPool {
		t.Error("a local agent that was told nothing about pooling does not pool, " +
			"so every flow it opens pays a handshake it need not")
	}
	opts, err = parseOptions(append(append([]string(nil), local...), "--quic-pool=false"))
	if err != nil {
		t.Fatal(err)
	}
	if opts.quicPool {
		t.Error("--quic-pool=false did not turn pooling off")
	}
}
