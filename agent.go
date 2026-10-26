package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// handshakeTimeout bounds starting an agent and every non-turn request
// (initialize, session/new, …). The first start of an npx-launched agent
// downloads the package, so it is generous.
const handshakeTimeout = 90 * time.Second

// agentProc is one running ACP agent process and its JSON-RPC connection.
type agentProc struct {
	name   string
	cmd    *exec.Cmd
	stdin  *os.File
	conn   *rpcConn
	init   acpInitializeResponse
	exited chan struct{}
	stderr *tailBuffer
	// stderrDone closes when the agent's stderr reached EOF.
	stderrDone chan struct{}
	// onStderr reads each stderr line too (nil = none; stderr.go).
	onStderr func(line string)
	// provider is the agent's environment provider (nil = none): it lives and
	// dies with this process.
	provider *envProvider
	// launch is the sandboxed launch the agent runs under (nil on the host); its
	// cleanups end with the agent.
	launch *sandboxLaunch
}

// startAgent spawns the agent in dir, runs initialize (advertising the
// client fs and terminal methods the policy answers) and, when configured,
// authenticate, within ctx's deadline. h receives everything the agent sends
// the client.
func startAgent(ctx context.Context, a AgentConfig, args []string, dir string, h rpcHandler, extraEnv ...string) (*agentProc, error) {
	return startAgentIn(ctx, a, args, dir, nil, h, extraEnv...)
}

// startAgentIn is startAgent for a host session (sb nil) or a sandboxed one:
// then the agent is started through `vmexec confine` in the workdir, with the
// descriptor's environment, and is not offered the client terminals (the
// adapter would run their commands on the host; the agent uses its own tools,
// which are confined and routed).
func startAgentIn(ctx context.Context, a AgentConfig, args []string, dir string, sb *sandboxLaunch, h rpcHandler, extraEnv ...string) (*agentProc, error) {
	path, err := exec.LookPath(a.Command)
	if err != nil {
		return nil, fmt.Errorf("command %q not found: %w", a.Command, err)
	}
	// The environment provider runs first, on the host and never confined, even
	// for a sandboxed session: it is where the credential lives, and the agent
	// is given only what it prints.
	var provider *envProvider
	if a.EnvProvider != nil {
		var providerEnv []string
		if provider, providerEnv, err = startEnvProvider(ctx, a); err != nil {
			return nil, err
		}
		extraEnv = append(append([]string(nil), extraEnv...), providerEnv...)
	}
	var cmd *exec.Cmd
	if sb != nil {
		cmd = sb.command(path, args)
		cmd.Env = append(a.sandboxedEnviron(extraEnv...), sb.env...)
	} else {
		cmd = exec.Command(path, args...)
		cmd.Dir = dir
		cmd.Env = a.environ(extraEnv...)
	}
	ownProcessGroup(cmd)
	// Plain os.Pipes (not cmd.*Pipe) so cmd.Wait never closes the read end
	// before the connection drained it.
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		return nil, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		outR.Close()
		outW.Close()
		return nil, err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, errW
	if err := cmd.Start(); err != nil {
		for _, f := range []*os.File{inR, inW, outR, outW, errR, errW} {
			f.Close()
		}
		if provider != nil {
			provider.close()
		}
		if sb != nil {
			sb.release()
		}
		return nil, fmt.Errorf("start %s: %w", a.Name, err)
	}
	inR.Close()
	outW.Close()
	errW.Close()
	p := &agentProc{name: a.Name, cmd: cmd, stdin: inW, exited: make(chan struct{}), stderr: &tailBuffer{max: 4 << 10}, stderrDone: make(chan struct{}), provider: provider, launch: sb}
	if o, ok := h.(stderrObserver); ok && len(a.StderrActivity) > 0 {
		p.onStderr = o.agentStderr
	}
	go p.logStderr(errR)
	p.conn = newRPCConn(outR, inW, h, a.Name)
	go func() {
		_ = cmd.Wait()
		close(p.exited)
	}()
	if provider != nil {
		go provider.watch(p)
	}
	if err := p.handshake(ctx, a, sb == nil); err != nil {
		p.stop()
		return nil, p.diagnose(err)
	}
	return p, nil
}

func (p *agentProc) handshake(ctx context.Context, a AgentConfig, terminal bool) error {
	req := acpInitializeRequest{
		ProtocolVersion:    acpProtocolVersion,
		ClientCapabilities: acpClientCapabilities{FS: acpFSCapabilities{ReadTextFile: true, WriteTextFile: true}, Terminal: terminal},
		ClientInfo:         acpImplementation{Name: "bnbloc-acp", Version: adapterVersion},
	}
	if err := p.call(ctx, methodInitialize, req, &p.init); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return errors.New("initialize: the agent did not answer in time (a first npx start downloads the package; try again)")
		}
		return fmt.Errorf("initialize: %w", err)
	}
	if p.init.ProtocolVersion != acpProtocolVersion {
		return fmt.Errorf("agent speaks ACP protocol version %d; the adapter speaks %d", p.init.ProtocolVersion, acpProtocolVersion)
	}
	if a.AuthMethod != "" {
		if err := p.call(ctx, methodAuthenticate, acpAuthenticateRequest{MethodID: a.AuthMethod}, nil); err != nil {
			return fmt.Errorf("authenticate %q: %w", a.AuthMethod, err)
		}
	}
	return nil
}

// call is a request that also fails fast when the agent process exits.
func (p *agentProc) call(ctx context.Context, method string, params, out any) error {
	err := p.conn.Call(ctx, method, params, out)
	if err != nil && errors.Is(err, errConnClosed) {
		return errAgentExited
	}
	return err
}

var errAgentExited = errors.New("the agent process exited")

// diagnose adds the agent's last stderr line to an exit error. Stderr is
// arbitrary local output: it goes to the adapter log and `bnbloc-acp check`,
// never over the wire.
func (p *agentProc) diagnose(err error) error {
	if !errors.Is(err, errAgentExited) {
		return err
	}
	select { // the exit's last words may still be in the pipe
	case <-p.stderrDone:
	case <-time.After(time.Second):
	}
	if last := p.stderr.lastLine(); last != "" {
		slog.Warn("bnbloc-acp agent exited", "agent", p.name, "stderr", truncate(last, 300))
		return &exitError{err: err, stderr: truncate(last, 300)}
	}
	return err
}

// exitError is an agent exit with its last stderr line; Error() omits the
// line (it may reach a room through a drive failure), Local() includes it.
type exitError struct {
	err    error
	stderr string
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }
func (e *exitError) Local() string { return e.err.Error() + ": " + e.stderr }

// localReason renders an error for local diagnostics, stderr included.
func localReason(err error) string {
	var x *exitError
	if errors.As(err, &x) {
		return x.Local()
	}
	return err.Error()
}

func (p *agentProc) logStderr(f *os.File) {
	defer close(p.stderrDone)
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		p.stderr.add(line)
		slog.Debug("bnbloc-acp agent stderr", "agent", p.name, "line", truncate(line, 500))
		if p.onStderr != nil {
			p.onStderr(line)
		}
	}
}

// alive reports whether the process still runs.
func (p *agentProc) alive() bool {
	select {
	case <-p.exited:
		return false
	default:
		return true
	}
}

// stop ends the agent: close its stdin (an ACP agent exits on EOF), then
// SIGTERM, then SIGKILL, a couple of seconds apart.
func (p *agentProc) stop() {
	defer func() {
		// Its endpoint goes with the agent.
		if p.provider != nil {
			p.provider.close()
		}
		if p.launch != nil {
			p.launch.release()
		}
	}()
	_ = p.stdin.Close()
	for _, step := range []func(){func() {}, func() { killTree(p.cmd, false) }, func() { killTree(p.cmd, true) }} {
		step()
		select {
		case <-p.exited:
			return
		case <-time.After(2 * time.Second):
		}
	}
}

// tailBuffer keeps the last max bytes of an agent's stderr.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf string
}

func (t *tailBuffer) add(line string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf += line + "\n"
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
}

func (t *tailBuffer) lastLine() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	lines := strings.Split(strings.TrimSpace(t.buf), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
