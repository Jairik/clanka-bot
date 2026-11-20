package lights

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// How long to wait before starting the helper again after it dies.
// The wait doubles on each failure so a crash loop cannot spin.
const (
	helperBackoffMin = 100 * time.Millisecond
	helperBackoffMax = 2 * time.Second
	// closeBudget bounds the follow-up that drops one device after a deadline.
	// The helper process itself stays up.
	closeBudget = 2 * time.Second
)

// bleRequest is one line sent to the long-lived helper.
type bleRequest struct {
	ID      int    `json:"id"`
	Op      string `json:"op"`
	Address string `json:"address,omitempty"`
	Char    string `json:"char,omitempty"`
	Hex     string `json:"hex,omitempty"`
}

// bleResponse is one line read back from the helper.
type bleResponse struct {
	ID      int             `json:"id"`
	OK      bool            `json:"ok"`
	Error   string          `json:"error,omitempty"`
	Devices json.RawMessage `json:"devices,omitempty"`
}

// bleOneShot reports whether the cold one-shot helper is selected.
// CLANKA_LIGHTS_BLE_ONESHOT=1 keeps a fresh python3 process per command.
// The default is the long-lived helper.
func bleOneShot() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CLANKA_LIGHTS_BLE_ONESHOT"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// bleRequestFrom turns the existing scan and write arguments into one JSON command.
func bleRequestFrom(args []string) (bleRequest, error) {
	if len(args) == 1 && args[0] == "scan" {
		return bleRequest{Op: "scan"}, nil
	}
	if len(args) == 3 {
		return bleRequest{Op: "write", Address: args[0], Char: args[1], Hex: args[2]}, nil
	}
	return bleRequest{}, errors.New("invalid ble helper request")
}

// runHelper sends one command to the long-lived process and returns its payload.
// A scan returns the device JSON array. A write returns no bytes.
func (s *Service) runHelper(ctx context.Context, args ...string) ([]byte, error) {
	req, err := bleRequestFrom(args)
	if err != nil {
		return nil, err
	}
	resp, err := s.callHelper(ctx, req)
	if err != nil && ctx.Err() != nil {
		// The caller's deadline is the reason, including when close ran afterwards.
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	// A well-formed answer means the process is healthy, even if the device is not.
	s.noteHelperOK()
	if !resp.OK {
		msg := resp.Error
		if msg == "" {
			msg = "light write failed"
		}
		return nil, errors.New(msg)
	}
	if req.Op == "scan" {
		if len(resp.Devices) == 0 {
			return []byte("[]"), nil
		}
		return resp.Devices, nil
	}
	return nil, nil
}

// callHelper starts the process on demand and matches one response by id.
// A deadline closes that device's client and leaves the process running.
func (s *Service) callHelper(ctx context.Context, req bleRequest) (bleResponse, error) {
	proc, err := s.ensureHelper(ctx)
	if err != nil {
		return bleResponse{}, err
	}
	// One user command at a time. Close is sent from inside this lock so it
	// can interrupt the command that just timed out.
	proc.reqMu.Lock()
	defer proc.reqMu.Unlock()
	resp, err := proc.exchange(ctx, req)
	if err != nil && ctx.Err() != nil && req.Op == "write" && req.Address != "" {
		closeCtx, cancel := context.WithTimeout(context.Background(), closeBudget)
		_, closeErr := proc.exchange(closeCtx, bleRequest{Op: "close", Address: req.Address})
		cancel()
		if closeErr != nil {
			// The helper did not take the close, so the next command needs a new process.
			s.retireHelper(proc)
		}
		return bleResponse{}, ctx.Err()
	}
	if err != nil {
		s.retireHelper(proc)
		return bleResponse{}, err
	}
	return resp, nil
}

// ensureHelper returns the running helper, starting one after backoff when the last one died.
func (s *Service) ensureHelper(ctx context.Context) (*bleProc, error) {
	s.helperMu.Lock()
	if s.proc != nil && s.proc.alive() {
		proc := s.proc
		s.helperMu.Unlock()
		return proc, nil
	}
	old := s.proc
	s.proc = nil
	wait := time.Duration(0)
	if s.helperFailed {
		wait = s.armBackoffLocked()
	}
	s.helperMu.Unlock()

	if old != nil {
		old.stop()
	}
	if wait > 0 {
		if err := sleepCtx(ctx, wait); err != nil {
			return nil, err
		}
	}
	return s.startHelperProc()
}

// startHelperProc launches the helper unless another start already won.
func (s *Service) startHelperProc() (*bleProc, error) {
	s.helperMu.Lock()
	defer s.helperMu.Unlock()
	if s.proc != nil && s.proc.alive() {
		return s.proc, nil
	}
	proc, err := startBleProc(s.helperCmd())
	if err != nil {
		s.helperFailed = true
		return nil, err
	}
	s.proc = proc
	return proc, nil
}

// helperCmd is the long-lived process. No arguments: the script reads stdin.
func (s *Service) helperCmd() *exec.Cmd {
	if s.startHelper != nil {
		return s.startHelper()
	}
	return exec.Command("python3", "-c", helper)
}

// armBackoffLocked returns the wait before the next start and schedules a longer one after that.
func (s *Service) armBackoffLocked() time.Duration {
	wait := s.backoff
	if wait == 0 {
		wait = s.restartDelay
	}
	if wait == 0 {
		wait = helperBackoffMin
	}
	next := wait * 2
	if next > helperBackoffMax {
		next = helperBackoffMax
	}
	s.backoff = next
	return wait
}

// noteHelperOK clears the crash backoff after a well-formed response.
func (s *Service) noteHelperOK() {
	s.helperMu.Lock()
	s.helperFailed = false
	s.backoff = 0
	s.helperMu.Unlock()
}

// retireHelper forgets a process that exited or stopped speaking the protocol.
func (s *Service) retireHelper(proc *bleProc) {
	s.helperMu.Lock()
	if s.proc == proc {
		s.proc = nil
	}
	s.helperFailed = true
	s.helperMu.Unlock()
	if proc != nil {
		proc.stop()
	}
}

// stopHelper ends the helper, if one is running. Tests use it so processes do not leak.
func (s *Service) stopHelper() {
	s.helperMu.Lock()
	proc := s.proc
	s.proc = nil
	s.helperMu.Unlock()
	if proc != nil {
		proc.stop()
	}
}

// sleepCtx waits for d or for the caller's deadline, whichever comes first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// bleProc is one running helper and the readers that pair its replies to requests.
type bleProc struct {
	cmd        *exec.Cmd
	stdin      io.WriteCloser
	reqMu      sync.Mutex
	mu         sync.Mutex
	seq        int
	waiters    map[int]chan bleResponse
	dead       chan struct{}
	once       sync.Once
	err        error
	stderr     []byte
	stderrDone chan struct{}
}

// startBleProc starts cmd and the loops that read its stdout and wait for exit.
func startBleProc(cmd *exec.Cmd) (*bleProc, error) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	proc := &bleProc{
		cmd:        cmd,
		stdin:      stdin,
		waiters:    map[int]chan bleResponse{},
		dead:       make(chan struct{}),
		stderrDone: make(chan struct{}),
	}
	// Stderr must be drained or a chatty import can fill the pipe and stall the helper.
	go func() {
		buf, _ := io.ReadAll(stderr)
		proc.mu.Lock()
		proc.stderr = buf
		proc.mu.Unlock()
		close(proc.stderrDone)
	}()
	go func() {
		err := cmd.Wait()
		if err == nil {
			err = errors.New("ble helper exited")
		}
		proc.fail(err)
	}()
	go proc.readLoop(stdout)
	return proc, nil
}

// alive reports whether the process is still the one we started.
func (p *bleProc) alive() bool {
	select {
	case <-p.dead:
		return false
	default:
		return true
	}
}

// exchange writes one request and waits for the reply with the same id.
func (p *bleProc) exchange(ctx context.Context, req bleRequest) (bleResponse, error) {
	if !p.alive() {
		return bleResponse{}, p.err
	}
	p.mu.Lock()
	p.seq++
	req.ID = p.seq
	ch := make(chan bleResponse, 1)
	p.waiters[req.ID] = ch
	id := req.ID
	p.mu.Unlock()

	payload, err := json.Marshal(req)
	if err != nil {
		p.forget(id)
		return bleResponse{}, err
	}
	// The write is outside the lock so a full pipe cannot block the reader.
	if _, err := p.stdin.Write(append(payload, '\n')); err != nil {
		p.forget(id)
		p.fail(err)
		return bleResponse{}, err
	}
	select {
	case resp := <-ch:
		return resp, nil
	case <-ctx.Done():
		p.forget(id)
		return bleResponse{}, ctx.Err()
	case <-p.dead:
		p.forget(id)
		if p.err == nil {
			return bleResponse{}, errors.New("ble helper exited")
		}
		return bleResponse{}, p.err
	}
}

// forget drops a waiter so a late line cannot be delivered to a finished call.
func (p *bleProc) forget(id int) {
	p.mu.Lock()
	delete(p.waiters, id)
	p.mu.Unlock()
}

// readLoop pairs each stdout line with the call that asked for it.
// A line that is not a reply means the process is no longer trustworthy.
func (p *bleProc) readLoop(stdout io.Reader) {
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var resp bleResponse
		if err := json.Unmarshal(sc.Bytes(), &resp); err != nil || resp.ID == 0 {
			p.fail(errors.New("ble helper protocol error"))
			return
		}
		p.mu.Lock()
		ch := p.waiters[resp.ID]
		delete(p.waiters, resp.ID)
		p.mu.Unlock()
		if ch != nil {
			ch <- resp
		}
	}
	if err := sc.Err(); err != nil {
		p.fail(err)
		return
	}
	p.fail(errors.New("ble helper exited"))
}

// fail records the process exit once and wakes every waiter.
// Killing a process that already exited is harmless.
func (p *bleProc) fail(err error) {
	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	p.once.Do(func() {
		select {
		case <-p.stderrDone:
			// The drain finished, so the last Bleak line can replace a bare exit status.
			if msg := p.stderrText(); msg != "" {
				err = errors.New(msg)
			}
		case <-time.After(50 * time.Millisecond):
		}
		if err == nil {
			err = errors.New("ble helper exited")
		}
		p.err = err
		close(p.dead)
	})
}

// stderrText keeps the helper's own last line, which is where Bleak states the cause.
func (p *bleProc) stderrText() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	text := strings.TrimSpace(string(p.stderr))
	if text == "" {
		return ""
	}
	lines := strings.Split(text, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		if k := strings.Index(line, ": "); k > 0 && strings.HasSuffix(line[:k], "Error") {
			line = line[k+2:]
		}
		return trimDetail(line, 140)
	}
	return ""
}

// stop ends the process and waits until the wait loop has reaped it.
func (p *bleProc) stop() {
	if p.stdin != nil {
		_ = p.stdin.Close()
	}
	p.fail(errors.New("ble helper stopped"))
	select {
	case <-p.dead:
	case <-time.After(2 * time.Second):
	}
}
