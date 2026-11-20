package lights

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/settings"
)

// CLANKA_FAKE_BLE_HELPER selects the child-process script inside TestMain.
// CLANKA_FAKE_BLE_REPORT is an optional file that script appends notes to.
const (
	fakeHelperEnv = "CLANKA_FAKE_BLE_HELPER"
	fakeReportEnv = "CLANKA_FAKE_BLE_REPORT"
)

// TestMain turns the test binary into the fake helper when the env var is set.
// The parent tests start that binary and talk to it over stdin and stdout.
func TestMain(m *testing.M) {
	if mode := os.Getenv(fakeHelperEnv); mode != "" {
		runFakeBLEHelper(mode)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeHelperCmd starts one fake helper mode and, when report is set, a note file.
func fakeHelperCmd(mode, report string) *exec.Cmd {
	cmd := exec.Command(os.Args[0])
	env := append(os.Environ(), fakeHelperEnv+"="+mode)
	if report != "" {
		env = append(env, fakeReportEnv+"="+report)
	}
	cmd.Env = env
	return cmd
}

// newFakeService is a lights service whose helper is the fake process.
func newFakeService(t *testing.T, start func() *exec.Cmd) *Service {
	t.Helper()
	t.Setenv("CLANKA_LIGHTS_BLE_ONESHOT", "")
	s := New(nil)
	s.startHelper = start
	s.restartDelay = 40 * time.Millisecond
	t.Cleanup(s.stopHelper)
	return s
}

// TestBLEOneShotFlag keeps the long-lived helper as the default.
func TestBLEOneShotFlag(t *testing.T) {
	t.Setenv("CLANKA_LIGHTS_BLE_ONESHOT", "")
	if bleOneShot() {
		t.Fatal("the default must be the long-lived helper")
	}
	for _, value := range []string{"1", "true", "yes", "on"} {
		t.Setenv("CLANKA_LIGHTS_BLE_ONESHOT", value)
		if !bleOneShot() {
			t.Fatalf("%q must select the one-shot helper", value)
		}
	}
}

// TestHelperFraming checks one JSON command in and one JSON result out.
func TestHelperFraming(t *testing.T) {
	report := filepath.Join(t.TempDir(), "notes.txt")
	s := newFakeService(t, func() *exec.Cmd {
		return fakeHelperCmd("ok", report)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	out, err := s.runWithin(ctx, "scan")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	var found []Device
	if err := json.Unmarshal(out, &found); err != nil {
		t.Fatalf("scan json %q: %v", out, err)
	}
	if len(found) != 1 || found[0].Address != "AA:BB:CC:DD:EE:01" || found[0].Name != "Desk" {
		t.Fatalf("scan = %+v", found)
	}

	if _, err := s.runWithin(ctx, "AA:BB:CC:DD:EE:01", "fff3", "7e00"); err != nil {
		t.Fatalf("write: %v", err)
	}
	notes := readNotes(t, report)
	if !strings.Contains(notes, "write AA:BB:CC:DD:EE:01 fff3 7e00") {
		t.Fatalf("notes = %q, want the write fields", notes)
	}
}

// TestHelperDeadlineLeavesTheProcessRunning times out one command, asks the
// helper to drop that device, and then completes a second command on the same process.
func TestHelperDeadlineLeavesTheProcessRunning(t *testing.T) {
	report := filepath.Join(t.TempDir(), "notes.txt")
	var starts atomic.Int32
	s := newFakeService(t, func() *exec.Cmd {
		starts.Add(1)
		return fakeHelperCmd("block", report)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, err := s.runWithin(ctx, "AA:BB:CC:DD:EE:01", "fff3", "01")
	if err == nil || ctx.Err() == nil {
		t.Fatalf("err = %v, want the deadline", err)
	}

	next, cancelNext := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelNext()
	if _, err := s.runWithin(next, "AA:BB:CC:DD:EE:01", "fff3", "02"); err != nil {
		t.Fatalf("second write after the deadline: %v", err)
	}
	if got := starts.Load(); got != 1 {
		t.Fatalf("helper starts = %d, want the same process", got)
	}
	notes := readNotes(t, report)
	if !strings.Contains(notes, "close AA:BB:CC:DD:EE:01") {
		t.Fatalf("notes = %q, want a close for the timed-out device", notes)
	}
}

// TestHelperRestartsAfterACrash starts a new process, after backoff, once the first one exits.
func TestHelperRestartsAfterACrash(t *testing.T) {
	var mu sync.Mutex
	var starts []time.Time
	s := newFakeService(t, func() *exec.Cmd {
		mu.Lock()
		starts = append(starts, time.Now())
		n := len(starts)
		mu.Unlock()
		mode := "ok"
		if n == 1 {
			mode = "crash"
		}
		return fakeHelperCmd(mode, "")
	})
	s.restartDelay = 60 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := s.runWithin(ctx, "scan"); err == nil {
		t.Fatal("the crashed helper must fail the in-flight command")
	}
	out, err := s.runWithin(ctx, "scan")
	if err != nil {
		t.Fatalf("restarted helper: %v", err)
	}
	if !strings.Contains(string(out), "AA:BB:CC:DD:EE:01") {
		t.Fatalf("scan after restart = %s", out)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(starts) != 2 {
		t.Fatalf("starts = %d, want a fresh process after the crash", len(starts))
	}
	if gap := starts[1].Sub(starts[0]); gap < 50*time.Millisecond {
		t.Fatalf("restart gap = %s, want the backoff", gap)
	}
}

// TestHelperRestartsAfterAProtocolError replaces a process that stops speaking JSON.
func TestHelperRestartsAfterAProtocolError(t *testing.T) {
	var starts atomic.Int32
	s := newFakeService(t, func() *exec.Cmd {
		n := starts.Add(1)
		mode := "ok"
		if n == 1 {
			mode = "garbage"
		}
		return fakeHelperCmd(mode, "")
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := s.runWithin(ctx, "scan"); err == nil {
		t.Fatal("a non-JSON line must fail the command")
	}
	if _, err := s.runWithin(ctx, "scan"); err != nil {
		t.Fatalf("helper after a protocol error: %v", err)
	}
	if got := starts.Load(); got != 2 {
		t.Fatalf("starts = %d, want a replacement process", got)
	}
}

// TestHelperSerialisesConcurrentWrites checks two overlapping callers never
// reach the helper together. The fake records an overlap if they do.
func TestHelperSerialisesConcurrentWrites(t *testing.T) {
	report := filepath.Join(t.TempDir(), "notes.txt")
	s := newFakeService(t, func() *exec.Cmd {
		return fakeHelperCmd("hold", report)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.runWithin(ctx, "AA:BB:CC:DD:EE:01", "fff3", "01")
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent write: %v", err)
		}
	}
	notes := readNotes(t, report)
	if strings.Contains(notes, "start 2") {
		t.Fatalf("notes = %q, want one write at a time", notes)
	}
	if strings.Count(notes, "start 1") != 2 {
		t.Fatalf("notes = %q, want two serial writes", notes)
	}
}

// TestHelperErrorReachesHealth keeps the helper's reason for the named controller.
func TestHelperErrorReachesHealth(t *testing.T) {
	ss := settingsWith(t, Device{Address: "AA:BB:CC:DD:EE:01", Name: "Desk", Protocol: "lotus"})
	s := New(func() *settings.Service { return ss })
	s.startHelper = func() *exec.Cmd { return fakeHelperCmd("error", "") }
	s.restartDelay = time.Millisecond
	t.Cleanup(s.stopHelper)
	t.Setenv("CLANKA_LIGHTS_BLE_ONESHOT", "")

	if _, err := s.Apply(context.Background(), "AA:BB:CC:DD:EE:01", "power", "on"); err == nil {
		t.Fatal("a helper error must fail the write")
	}
	want := "Desk: failed to discover services, device disconnected"
	if s.Last() != want || !s.Failed() {
		t.Fatalf("Last() = %q, Failed() = %v, want %q and true", s.Last(), s.Failed(), want)
	}
}

func readNotes(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read notes: %v", err)
	}
	return string(raw)
}

// fakeReq is the subset of a helper command the fake understands.
type fakeReq struct {
	ID      int    `json:"id"`
	Op      string `json:"op"`
	Address string `json:"address"`
	Char    string `json:"char"`
	Hex     string `json:"hex"`
}

// runFakeBLEHelper speaks the helper protocol without Bluetooth.
// hold answers writes from a goroutine so an overlapping pair is visible.
// block waits to be closed. crash exits. garbage prints a bad line and stays up.
func runFakeBLEHelper(mode string) {
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	report := os.Getenv(fakeReportEnv)
	var noteMu sync.Mutex
	note := func(msg string) {
		if report == "" {
			return
		}
		noteMu.Lock()
		defer noteMu.Unlock()
		f, err := os.OpenFile(report, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return
		}
		fmt.Fprintln(f, msg)
		_ = f.Close()
	}
	var outMu sync.Mutex
	respond := func(v any) {
		outMu.Lock()
		defer outMu.Unlock()
		enc := json.NewEncoder(os.Stdout)
		_ = enc.Encode(v)
	}

	var inflight atomic.Int32
	var blocked atomic.Bool
	release := make(chan struct{})
	var releaseOnce sync.Once
	signal := func() { releaseOnce.Do(func() { close(release) }) }

	for in.Scan() {
		var req fakeReq
		if err := json.Unmarshal(in.Bytes(), &req); err != nil {
			fmt.Fprintln(os.Stdout, "not-json")
			return
		}
		switch mode {
		case "crash":
			note("crash")
			os.Exit(1)
		case "garbage":
			fmt.Fprintln(os.Stdout, "not json")
			select {}
		}
		if req.Op == "close" {
			note("close " + req.Address)
			signal()
			respond(map[string]any{"id": req.ID, "ok": true})
			continue
		}
		if req.Op == "write" && mode == "hold" {
			n := inflight.Add(1)
			note(fmt.Sprintf("start %d", n))
			go func(req fakeReq, n int32) {
				time.Sleep(80 * time.Millisecond)
				inflight.Add(-1)
				note(fmt.Sprintf("end %d", n))
				respond(map[string]any{"id": req.ID, "ok": true})
			}(req, n)
			continue
		}
		if req.Op == "write" && mode == "block" && blocked.CompareAndSwap(false, true) {
			note("blocked " + req.Address)
			go func(req fakeReq) {
				<-release
				respond(map[string]any{"id": req.ID, "ok": false, "error": "cancelled"})
			}(req)
			continue
		}
		resp := map[string]any{"id": req.ID, "ok": true}
		if mode == "error" && req.Op == "write" {
			resp["ok"] = false
			resp["error"] = "failed to discover services, device disconnected"
		}
		if req.Op == "scan" {
			resp["devices"] = []map[string]string{{"address": "AA:BB:CC:DD:EE:01", "name": "Desk"}}
		}
		if req.Op == "write" {
			note("write " + req.Address + " " + req.Char + " " + req.Hex)
		}
		respond(resp)
	}
}
