package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// An environment provider is a program the core runs for an agent, on the host
// and never confined, whose output becomes part of the agent's environment. It
// is how an agent reaches a service whose address and credentials cannot be
// written in a file: a local proxy that starts with a new port each time, a
// token that is renewed. The agent itself keeps its own command, so everything
// the core recognises from it (the sandbox routing above all) applies as it
// would without a provider.
//
// What it must do:
//   - print `NAME=VALUE` lines on stdout, one per variable, then a line
//     `READY`; anything it prints after READY is ignored;
//   - keep running while the agent does, and exit when its stdin closes (the
//     core closes it when the agent's process ends, for any reason, and ends
//     the process group after a short wait).
//
// The core never logs a value, adds the variables to the agent's environment
// after the agent's own `env` and before the sandbox descriptor's (which wins),
// refuses names it owns, and ends the agent if the provider ends first (its
// endpoint is gone; the next input starts both again).
//
// Why outside the sandbox: a sandboxed agent's network is open, so a
// credential it can read is a credential it can send away. The provider is
// where the credential lives; the agent is given the endpoint and, at most, a
// placeholder.

// EnvProviderConfig is the provider's launch line. It is the member's own
// config (or a component's drop-in): the same trust as an agent's command.
type EnvProviderConfig struct {
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
}

const (
	// providerReady is the line that ends the provider's variables.
	providerReady = "READY"
	// providerStartTimeout bounds the provider's start (a proxy, a catalog); the
	// caller's context may be shorter.
	providerStartTimeout = 60 * time.Second
	// providerMaxVars and providerMaxBytes bound what a provider may add.
	providerMaxVars  = 128
	providerMaxBytes = 256 << 10
)

// envProvider is one running provider.
type envProvider struct {
	name   string
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	done   chan struct{} // closed when the process has exited
	stderr *tailBuffer
	once   sync.Once
}

// startEnvProvider runs the provider for agent a and returns it with the
// variables it printed. The caller owns the provider: close it when the agent's
// process ends.
func startEnvProvider(ctx context.Context, a AgentConfig) (*envProvider, []string, error) {
	cfg := a.EnvProvider
	path, err := exec.LookPath(cfg.Command)
	if err != nil {
		return nil, nil, fmt.Errorf("agent %q: environment provider %q not found: %w", a.Name, cfg.Command, err)
	}
	ctx, cancel := context.WithTimeout(ctx, providerStartTimeout)
	defer cancel()
	cmd := exec.Command(path, cfg.Args...)
	// The member's environment for the agent (its env: PATH and the like), not
	// the adapter's own settings: the provider is no more trusted with those
	// than the agent is.
	cmd.Env = a.environ()
	ownProcessGroup(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("agent %q: start the environment provider: %w", a.Name, err)
	}
	p := &envProvider{name: a.Name, cmd: cmd, stdin: stdin, done: make(chan struct{}), stderr: &tailBuffer{max: 4 << 10}}
	go func() {
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			p.stderr.add(sc.Text())
		}
	}()
	go func() {
		_ = cmd.Wait()
		close(p.done)
	}()

	type result struct {
		env []string
		err error
	}
	res := make(chan result, 1)
	go func() {
		env, err := readProviderEnv(stdout)
		res <- result{env, err}
	}()
	select {
	case r := <-res:
		if r.err != nil {
			p.close()
			return nil, nil, p.failure(r.err)
		}
		slog.Info("bnbloc-acp environment provider ready", "agent", a.Name, "variables", len(r.env))
		return p, r.env, nil
	case <-ctx.Done():
		p.close()
		return nil, nil, p.failure(fmt.Errorf("not ready in time: %w", ctx.Err()))
	}
}

// readProviderEnv reads `NAME=VALUE` lines up to READY, then drains the rest
// so the provider never blocks writing to us.
func readProviderEnv(r io.Reader) ([]string, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), providerMaxBytes+1)
	var env []string
	size := 0
	for sc.Scan() {
		line := sc.Text()
		if line == providerReady {
			go func() { _, _ = io.Copy(io.Discard, r) }()
			return env, nil
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok || !envNameRE.MatchString(name) {
			return nil, errors.New("printed a line that is not NAME=VALUE")
		}
		if strings.HasPrefix(name, "BNBLOC_ACP_") {
			return nil, fmt.Errorf("tried to set %s, which the core keeps from agents", name)
		}
		if strings.ContainsRune(value, 0) {
			return nil, fmt.Errorf("gave %s a value with a NUL", name)
		}
		size += len(line)
		if len(env) >= providerMaxVars || size > providerMaxBytes {
			return nil, errors.New("printed more than the core takes")
		}
		env = append(env, line)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("output unreadable: %w", err)
	}
	return nil, errors.New("exited before it printed READY")
}

// failure names why the provider could not start; its stderr's last line is
// local diagnostics, like an agent's: the adapter log and `check`, never the
// wire (exitError keeps it out of Error()).
func (p *envProvider) failure(err error) error {
	select { // the exit's last words may still be in the pipe
	case <-p.done:
		time.Sleep(50 * time.Millisecond)
	case <-time.After(300 * time.Millisecond):
	}
	base := fmt.Errorf("agent %q: the environment provider %w", p.name, err)
	if last := p.stderr.lastLine(); last != "" {
		slog.Warn("bnbloc-acp environment provider failed", "agent", p.name, "stderr", truncate(last, 300))
		return &exitError{err: base, stderr: truncate(last, 300)}
	}
	return base
}

// alive reports whether the provider still runs.
func (p *envProvider) alive() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

// close ends the provider: stdin closes (it exits on EOF), then its process
// group is ended after a short wait. Safe to call more than once.
func (p *envProvider) close() {
	p.once.Do(func() {
		_ = p.stdin.Close()
		for _, step := range []func(){func() {}, func() { killTree(p.cmd, false) }, func() { killTree(p.cmd, true) }} {
			step()
			select {
			case <-p.done:
				return
			case <-time.After(2 * time.Second):
			}
		}
	})
}

// watch ends the agent when the provider ends before it: the agent's endpoint
// is gone, and the next input starts both again. It returns when either ends.
func (p *envProvider) watch(agent *agentProc) {
	select {
	case <-p.done:
		if agent.alive() {
			slog.Warn("bnbloc-acp environment provider ended before its agent; ending the agent", "agent", p.name)
			killTree(agent.cmd, false)
		}
	case <-agent.exited:
		p.close()
	}
}
