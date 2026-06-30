// Package proc provides subprocess lifecycle management mirroring the Python
// api/process_utils.py helpers, but using reliable liveness detection.
//
// Liveness in the Python version relied on Popen.poll(); here we run cmd.Wait()
// in a goroutine and close a done channel, so Exited()/Done() never report a
// zombie as alive (unlike Signal(0)/kill -0 polling).
package proc

import (
	"bufio"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// NewStdoutPipe wires an os.Pipe to cmd.Stdout and returns both ends.
//
// We deliberately avoid cmd.StdoutPipe(): its reader is closed by cmd.Wait(),
// which races with concurrent reads. With a raw os.Pipe the parent owns both
// fds. After Start the parent MUST Close w (so only the child holds the write
// end and the reader sees EOF when the child exits); the reader Closes r when
// it is done.
func NewStdoutPipe(cmd *exec.Cmd) (r, w *os.File, err error) {
	r, w, err = os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	cmd.Stdout = w
	return r, w, nil
}

// NewStdinPipe wires an os.Pipe to cmd.Stdin and returns both ends. After Start
// the parent MUST Close r (the child holds its own copy); the parent writes to
// w and Closes w to signal EOF to the child.
func NewStdinPipe(cmd *exec.Cmd) (r, w *os.File, err error) {
	r, w, err = os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	cmd.Stdin = r
	return r, w, nil
}

// NewStderrPipe wires an os.Pipe to cmd.Stderr and returns both ends. Same
// contract as NewStdoutPipe: the parent Closes w after Start, then drains r.
// Avoids cmd.StderrPipe()'s Wait-closes-the-pipe hazard.
func NewStderrPipe(cmd *exec.Cmd) (r, w *os.File, err error) {
	r, w, err = os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	cmd.Stderr = w
	return r, w, nil
}

// Proc wraps an *exec.Cmd, tracking its liveness via a done channel that is
// closed when cmd.Wait() returns.
type Proc struct {
	cmd  *exec.Cmd
	done chan struct{}

	mu       sync.Mutex
	exited   bool
	waitErr  error
	exitCode int
}

// Start configures the command to run in its own process group (so the whole
// group can be signalled later), starts it, and spawns a goroutine that calls
// cmd.Wait() and closes the done channel when the process exits.
func Start(cmd *exec.Cmd) (*Proc, error) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	// Setpgid puts the child in its own process group with PGID == PID, so we
	// can signal the entire group via kill(-pid). Mirrors start_new_session.
	cmd.SysProcAttr.Setpgid = true

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	p := &Proc{cmd: cmd, done: make(chan struct{})}
	go func() {
		err := cmd.Wait()
		p.mu.Lock()
		p.exited = true
		p.waitErr = err
		p.exitCode = cmd.ProcessState.ExitCode()
		p.mu.Unlock()
		close(p.done)
	}()
	return p, nil
}

// Done returns a channel that is closed once the process has exited.
func (p *Proc) Done() <-chan struct{} { return p.done }

// Exited reports whether the process has already exited.
func (p *Proc) Exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// Pid returns the process id.
func (p *Proc) Pid() int {
	if p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

// ExitCode returns the process exit code (valid after exit; -1 if unknown).
func (p *Proc) ExitCode() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.exited {
		return -1
	}
	return p.exitCode
}

// Terminate sends SIGTERM to the process group, waits up to timeout for a clean
// exit, then escalates to SIGKILL. Mirrors api/process_utils._terminate_process.
func (p *Proc) Terminate(timeout time.Duration) {
	if p == nil {
		return
	}
	if p.Exited() {
		slog.Info("process-already-exited", "pid", p.Pid(), "rc", p.ExitCode())
		return
	}

	pid := p.Pid()
	slog.Info("process-stop-start", "pid", pid)

	// Signal the whole group: kill(-pid, sig). ESRCH (group gone) is ignored.
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
		return
	}

	select {
	case <-p.done:
		slog.Info("process-stop-graceful", "pid", pid, "rc", p.ExitCode())
		return
	case <-time.After(timeout):
		slog.Warn("process-stop-timeout", "pid", pid)
	}

	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		return
	}

	select {
	case <-p.done:
		slog.Info("process-stop-force", "pid", pid, "rc", p.ExitCode())
	case <-time.After(time.Second):
		slog.Error("process-stop-force-timeout", "pid", pid)
	}
}

// DrainStderr reads lines from r (typically a process stderr pipe) and mirrors
// each non-empty line into the application logs at WARN, prefixed by name.
// Mirrors api/process_utils._spawn_stderr_logger.
func DrainStderr(name string, r io.Reader) {
	go func() {
		scanner := bufio.NewScanner(r)
		// DV/ffmpeg can emit long lines; raise the buffer ceiling.
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			if line != "" {
				slog.Warn(name+"-stderr", "line", line)
			}
		}
	}()
}
