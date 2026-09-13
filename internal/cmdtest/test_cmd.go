// Copyright 2017 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package cmdtest

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"text/template"
	"time"

	"github.com/docker/docker/pkg/reexec"
)

func NewTestCmd(t *testing.T, data interface{}) *TestCmd {
	return &TestCmd{T: t, Data: data}
}

type TestCmd struct {
	// For total convenience, all testing methods are available.
	*testing.T

	Func    template.FuncMap
	Data    interface{}
	Cleanup func()

	cmd    *exec.Cmd
	stdout *bufio.Reader
	stdin  io.WriteCloser
	stderr *testlogger
	// stdoutLog collects the child's stdout as it is read, and stdoutPipe keeps the
	// read end open past Wait so WaitExit can drain what the Expect helpers left
	// behind. Wait closes the reader of a StdoutPipe, discarding exactly that text.
	stdoutLog     *testlogger
	stdoutPipe    *os.File
	stdoutMu      sync.Mutex
	stdoutDrained bool
	// stdoutTruncated records that the drain ended without seeing EOF, so the text
	// StdoutText returns may be missing a tail the child wrote. It is deliberately
	// not guarded by stdoutMu: StdoutText must not block behind a release or a bound.
	stdoutTruncated atomic.Bool
	// cleanupOnce keeps the caller's Cleanup hook to a single run. A helper timeout,
	// an ExpectExit and an explicit Kill can all reach it for the same child, and a
	// hook such as os.RemoveAll must not have to be idempotent.
	cleanupOnce sync.Once
	// waitStart, waitDone and waitErr carry the one exec.Cmd.Wait every path shares:
	// WaitExit, ExpectExit through it and the test cleanup must not enter Wait twice.
	// The wait runs unbounded in the background while each caller bounds its own wait
	// for the result, so a caller that gives up does not spend the wait for the
	// others, and the close of waitDone is what orders waitErr before a reader.
	waitStart sync.Once
	waitDone  chan struct{}
	waitErr   error
	// drainOnce and drainDone carry the one stdout copy every path shares, the way
	// waitStart carries the one Wait. A second WaitExit can arrive while the first
	// drain is still in flight - the pipe stays open for as long as a process the
	// child left behind holds it - and a second copy would read the same
	// bufio.Reader from another goroutine, which is not safe. drainDone is created
	// in Run and closed by the copy, so a later caller observes the same drain's end
	// instead of starting one of its own.
	drainOnce sync.Once
	drainDone chan struct{}
	// errMu serialises Err: Interrupt and a WaitExit called from another goroutine
	// write it from different threads. It guards the assignment alone - nothing that
	// can block runs under it - so ExitStatus stays a non-blocking read.
	errMu sync.Mutex
	// testDone makes the loggers stop calling t.Logf once the test has finished. A
	// process the child handed stderr to can outlive the test cleanup, and logging
	// from that goroutine panics with "Log in goroutine after ... has completed".
	// A logger reads this flag and then logs, so the flag cannot close that window -
	// it only makes it small, which is the most it can do from here.
	testDone atomic.Bool
	// holdWriteEnd keeps the parent's copy of the child's write end open after
	// Start, which Run drops otherwise so the read end reports EOF once the child
	// and every process it handed the descriptor to are gone. Only the test of the
	// drain's own bound sets it: a held write end is the shape a process outliving
	// the child leaves behind, and the drain cannot tell the two apart, but the
	// parent gives its copy back with the test instead of outliving the test binary
	// the way a spawned process does.
	holdWriteEnd bool
	heldWriteEnd *os.File
	// KillTimeout controls how long expect/wait helpers wait for child output before
	// killing the child, and how long WaitExit waits for the child to be reaped. Zero
	// value falls back to the historical default. It bounds neither the stdout drain
	// nor the test cleanup; see reapTimeout and drainTimeout for those.
	KillTimeout time.Duration
	// MaxLogSize caps how much of a child's stdout and stderr each logger keeps for
	// inspection. Zero falls back to defaultMaxLogSize. It applies to both streams:
	// the tail beyond the cap is dropped and text() says how many bytes went
	// missing. Run snapshots it into maxLogSize, so an assignment only counts before
	// Run; SetMaxLogSize changes the cap of a child that is already running.
	MaxLogSize int
	// maxLogSize is the snapshot the loggers read, atomically: exec.Cmd's stderr
	// copier and the stdout drain write the loggers from their own goroutines, so a
	// plain field would race with the test's assignment.
	maxLogSize atomic.Int64
	// Err holds what WaitExit last saw: the process exit error, or nil both when the
	// process exited cleanly and when the reap gave up inside its budget and
	// observed no exit at all. Use ExitObserved to tell those two apart before
	// reading ExitStatus; Interrupt's own error is overwritten by WaitExit.
	Err error
}

var id int32

// Run exec's the current binary using name as argv[0] which will trigger the
// reexec init function for that name (e.g. "geth-test" in cmd/geth/run_test.go)
func (tt *TestCmd) Run(name string, args ...string) {
	if tt.cmd != nil {
		// A second Run would overwrite cmd, stdoutPipe and heldWriteEnd: the first
		// child would never be reaped, its descriptors would stay open, and the
		// cleanup registered below only ever sees the last one. Deliberately not
		// pinned by a test: asserting a Fatal needs a test that fails on purpose,
		// which would leave a FAIL line in every run of this package.
		tt.Fatal("cmdtest: Run may only be called once per TestCmd")
	}
	id := atomic.AddInt32(&id, 1)
	number := fmt.Sprintf("%d", id)
	tt.testDone.Store(false)
	tt.maxLogSize.Store(int64(tt.MaxLogSize))
	tt.stderr = &testlogger{t: tt.T, name: number, stream: "stderr", logLines: true, maxLimit: tt.logLimit, tDone: &tt.testDone}
	tt.stdoutLog = &testlogger{t: tt.T, name: number, stream: "stdout", maxLimit: tt.logLimit, tDone: &tt.testDone}
	tt.stdoutDrained = false
	tt.waitDone = make(chan struct{})
	tt.drainDone = make(chan struct{})

	// An explicit pipe rather than StdoutPipe: the reader of a StdoutPipe is closed
	// by Wait, so text the Expect helpers did not consume would be unreachable for
	// assertions even though the child had already written it. Dropping the
	// parent's write end after Start leaves the read end reporting EOF once the
	// child and any process it handed the descriptor to are gone.
	pr, pw, err := os.Pipe()
	if err != nil {
		tt.Fatal(err)
	}
	tt.cmd = &exec.Cmd{
		Path:   reexec.Self(),
		Args:   append([]string{name}, args...),
		Stdout: pw,
		Stderr: tt.stderr,
	}
	tt.stdoutPipe = pr
	tt.stdout = bufio.NewReader(io.TeeReader(pr, tt.stdoutLog))
	// A test that only calls Run would otherwise leave the read end open and the
	// child unreaped until the test binary exits, so release both when the test
	// ends. WaitExit remains the normal path and makes this a no-op. Run already
	// dereferences tt.T through the embedded *testing.T (tt.Fatal above and below),
	// so a nil T is not a supported state and needs no guard here.
	tt.T.Cleanup(tt.releaseStdout)
	if tt.stdin, err = tt.cmd.StdinPipe(); err != nil {
		pw.Close()
		tt.Fatal(err)
	}
	if err := tt.cmd.Start(); err != nil {
		pw.Close()
		tt.Fatal(err)
	}
	if tt.holdWriteEnd {
		tt.heldWriteEnd = pw
	} else {
		pw.Close()
	}
}

// InputLine writes the given text to the childs stdin.
// This method can also be called from an expect template, e.g.:
//
//	geth.expect(`Passphrase: {{.InputLine "password"}}`)
func (tt *TestCmd) InputLine(s string) string {
	io.WriteString(tt.stdin, s+"\n")
	return ""
}

func (tt *TestCmd) SetTemplateFunc(name string, fn interface{}) {
	if tt.Func == nil {
		tt.Func = make(map[string]interface{})
	}
	tt.Func[name] = fn
}

// Expect runs its argument as a template, then expects the
// child process to output the result of the template within KillTimeout
// (30s by default).
//
// If the template starts with a newline, the newline is removed
// before matching.
func (tt *TestCmd) Expect(tplsource string) {
	// Generate the expected output by running the template.
	tpl := template.Must(template.New("").Funcs(tt.Func).Parse(tplsource))
	wantbuf := new(bytes.Buffer)
	if err := tpl.Execute(wantbuf, tt.Data); err != nil {
		panic(err)
	}
	// Trim exactly one newline at the beginning. This makes tests look
	// much nicer because all expect strings are at column 0.
	want := bytes.TrimPrefix(wantbuf.Bytes(), []byte("\n"))
	if err := tt.matchExactOutput(want); err != nil {
		tt.Fatal(err)
	}
	tt.Logf("Matched stdout text:\n%s", want)
}

func (tt *TestCmd) matchExactOutput(want []byte) error {
	buf := make([]byte, len(want))
	n := 0
	tt.withKillTimeout(func() { n, _ = io.ReadFull(tt.stdout, buf) })
	buf = buf[:n]
	if n < len(want) || !bytes.Equal(buf, want) {
		// Grab any additional buffered output in case of mismatch
		// because it might help with debugging.
		buf = append(buf, make([]byte, tt.stdout.Buffered())...)
		tt.stdout.Read(buf[n:])
		// Find the mismatch position.
		for i := 0; i < n; i++ {
			if want[i] != buf[i] {
				return fmt.Errorf("output mismatch at ◊:\n---------------- (stdout text)\n%s%s\n---------------- (expected text)\n%s",
					buf[:i], buf[i:n], want)
			}
		}
		if n < len(want) {
			return fmt.Errorf("not enough output, got until ◊:\n---------------- (stdout text)\n%s\n---------------- (expected text)\n%s◊%s",
				buf, want[:n], want[n:])
		}
	}
	return nil
}

// ExpectRegexp expects the child process to output text matching the
// given regular expression within KillTimeout (30s by default).
//
// Note that an arbitrary amount of output may be consumed by the
// regular expression. This usually means that expect cannot be used
// after ExpectRegexp.
func (tt *TestCmd) ExpectRegexp(regex string) (*regexp.Regexp, []string) {
	regex = strings.TrimPrefix(regex, "\n")
	var (
		re      = regexp.MustCompile(regex)
		rtee    = &runeTee{in: tt.stdout}
		matches []int
	)
	tt.withKillTimeout(func() { matches = re.FindReaderSubmatchIndex(rtee) })
	output := rtee.buf.Bytes()
	if matches == nil {
		tt.Fatalf("Output did not match:\n---------------- (stdout text)\n%s\n---------------- (regular expression)\n%s",
			output, regex)
		return re, nil
	}
	tt.Logf("Matched stdout text:\n%s", output)
	var submatches []string
	for i := 0; i < len(matches); i += 2 {
		submatch := string(output[matches[i]:matches[i+1]])
		submatches = append(submatches, submatch)
	}
	return re, submatches
}

// ExpectExit expects the child process to exit within KillTimeout (30s by
// default) without printing any additional text on stdout.
func (tt *TestCmd) ExpectExit() {
	var output []byte
	tt.withKillTimeout(func() {
		output, _ = io.ReadAll(tt.stdout)
	})
	tt.WaitExit()
	tt.runCleanup()
	if len(output) > 0 {
		tt.Errorf("Unmatched stdout text:\n%s", output)
	}
}

// While this call is in flight the drain is the only reader of tt.stdout, so no
// other goroutine may use Expect, ExpectRegexp or matchExactOutput concurrently.
// Once it returns the read end has been released and those helpers are no longer
// usable either; StdoutText is the concurrent-safe reader and does not wait on an
// in-flight drain.
func (tt *TestCmd) WaitExit() {
	// Drain in the background: waiting for the child first is what deadlocks. A
	// child that filled the pipe blocks in write until somebody reads it, and it
	// cannot exit while it blocks, so the two have to make progress at the same time.
	// Only the first caller starts the copy - the reader is not safe for two
	// goroutines - and every caller waits on that same drain.
	tt.drainOnce.Do(func() {
		go func() {
			defer close(tt.drainDone)
			tt.copyStdout()
		}()
	})
	// The reap follows the test's own budget rather than a fixed bound: callers read
	// ExitStatus() afterwards, and giving up early would report a command that
	// failed as a successful exit.
	tt.setErr(tt.reap(tt.killTimeout()))
	// Only now, with the child reaped, bound what is left of the copy: while the
	// child was running its output still had to be consumed so that it could exit,
	// but from here on the only source of more output is a process it left behind,
	// and waiting on that is what must not outlive drainTimeout.
	tt.boundStdout()
	<-tt.drainDone
}

// reap waits for the child once, bounded by the caller's own limit. Every path shares
// the one exec.Cmd.Wait, so a WaitExit racing the test cleanup cannot enter it twice.
// The budget belongs to the caller, and a caller that gives up does not spend the wait
// for the others: the wait keeps running in the background, so a later caller with a
// longer budget still observes the exit.
//
// The return value is the exit as this call saw it: a call that gave up returns nil,
// and ExitStatus re-reads the shared wait, so a late exit is still reported with its
// real status rather than as a success.
func (tt *TestCmd) reap(limit time.Duration) error {
	if tt.cmd == nil || tt.cmd.Process == nil {
		return nil
	}
	tt.waitStart.Do(func() {
		go func() {
			tt.waitErr = tt.cmd.Wait()
			close(tt.waitDone)
		}()
	})
	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case <-tt.waitDone:
		// The close above is what orders the write of waitErr before this read.
		return tt.waitErr
	case <-timer.C:
		tt.logf("child not reaped within %v; it is still running or a process it spawned still holds stderr", limit)
		// Deliberately no read of waitErr: the wait is still in flight, and only the
		// close gives the reader a happens-before edge.
		return nil
	}
}

// setErr records what this call observed. WaitExit may be called from more than one
// goroutine and Interrupt writes the field too, so the assignment is serialised; the
// value each caller reports is still its own observation, and ExitStatus re-reads the
// shared wait when a call gave up.
func (tt *TestCmd) setErr(err error) {
	tt.errMu.Lock()
	tt.Err = err
	tt.errMu.Unlock()
}

// errValue reads Err under the same lock. It waits for nothing else, so ExitStatus and
// ExitObserved stay non-blocking.
func (tt *TestCmd) errValue() error {
	tt.errMu.Lock()
	defer tt.errMu.Unlock()
	return tt.Err
}

// logf logs unless the test has already finished. The stdout copy, the reap and the
// helper timeout callback can all run on a goroutine that outlives the test - a
// process the child handed a pipe to can keep them alive past the cleanup - and a
// t.Logf from there panics with "Log in goroutine after ... has completed".
func (tt *TestCmd) logf(format string, args ...interface{}) {
	if tt.testDone.Load() {
		return
	}
	tt.Logf(format, args...)
}

// copyStdout consumes the child's stdout into stdoutLog until the pipe reports EOF,
// and then releases the read end. The tee under the reader writes every byte into
// stdoutLog as it is read, so copying into it is the whole job. This is what keeps a
// child that filled the pipe able to exit.
//
// It holds no lock across the copy: the bound that ends a copy nobody can finish has
// to reach the pipe while the copy runs, and boundStdout is where that happens.
func (tt *TestCmd) copyStdout() {
	tt.stdoutMu.Lock()
	reader, drained := tt.stdout, tt.stdoutDrained
	tt.stdoutMu.Unlock()
	if reader == nil || drained {
		return
	}
	if _, err := io.Copy(io.Discard, reader); err != nil {
		// The drain never reached EOF, so whatever the child wrote after the last
		// byte it read is missing from stdoutLog; StdoutText has to say so.
		tt.stdoutTruncated.Store(true)
		if errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, os.ErrClosed) {
			// The drain is bounded, and a release from the cleanup or a helper
			// timeout ends it just as a deadline does: log the error itself rather
			// than blaming a holder of the write end, which may not exist.
			tt.logf("stdout drain stopped: %v", err)
		} else {
			tt.logf("failed to drain stdout: %v", err)
		}
	}
	tt.stdoutMu.Lock()
	tt.releaseStdoutLocked()
	tt.stdoutMu.Unlock()
}

// boundStdout gives the copy above an upper bound. WaitExit calls it once the child
// is reaped, so the bound covers exactly the interval in which the only thing that
// can still produce output is a process the child left behind - a process that kills
// cannot influence, which is why the bound gives the read end back instead of
// killing anything. What the bound cuts off is dropped along with the read end; the
// text collected up to that point stays readable. A pipe that rejects deadlines,
// such as the one os.Pipe returns on Windows, gets the same bound from closing the
// read end once drainTimeout passes, which ends a blocked read just as the deadline
// does.
func (tt *TestCmd) boundStdout() {
	tt.stdoutMu.Lock()
	defer tt.stdoutMu.Unlock()
	if tt.stdoutPipe == nil || tt.stdoutDrained {
		return
	}
	pipe := tt.stdoutPipe
	if err := pipe.SetReadDeadline(time.Now().Add(drainTimeout)); err != nil {
		// The timer is deliberately not stopped, and must not take stdoutMu: the
		// copy it ends holds no lock either, and closing this *os.File again after
		// the copy released it is a no-op that cannot touch another descriptor.
		time.AfterFunc(drainTimeout, func() { pipe.Close() })
	}
}

// reapTimeout bounds the reap the test cleanup performs; drainTimeout bounds the
// drain. Neither is KillTimeout: that is the budget for output a test is waiting
// for and callers raise it to minutes, while a cleanup - and a drain nobody can
// unblock - must never hold the package that long.
const (
	reapTimeout  = 2 * time.Second
	drainTimeout = 2 * time.Second
)

// releaseStdout releases the child's stdout and reaps it if the test never waited
// for it. It is registered with the test's cleanup, so a test that only calls Run
// leaks neither the read end nor a zombie child.
func (tt *TestCmd) releaseStdout() {
	// Everything here runs around the end of the test, so the loggers should stop
	// calling t.Logf: the stderr copier can still be running and a t.Logf from its
	// goroutine once the test has finished panics. Flagging the end of the test makes
	// that window as small as it can be made from here, not impossible.
	defer tt.testDone.Store(true)

	if tt.cmd != nil && tt.cmd.Process != nil {
		tt.cmd.Process.Kill()
		// The reap waits for the child the kill above just terminated, but it also
		// waits for the stderr copier exec.Cmd installs when Stderr is not an
		// *os.File: that copier returns only once every holder of the stderr write
		// end is gone, and a process the child spawned can hold it far past the test.
		// reapTimeout bounds it, so a cleanup lets a zombie linger until the test
		// binary exits rather than hang the package until go test's own timeout.
		tt.reap(reapTimeout)
	}
	// exec.Cmd.Wait closes this write end, but the reap above can give up before Wait
	// completes - it waits for the stderr copier, which a process the child spawned
	// can hold past the test - and then the descriptor would stay open for the life of
	// the binary. The child is already killed by this point, so closing it cannot
	// change what the test observes.
	if tt.stdin != nil {
		tt.stdin.Close()
	}
	// Give the held write end back before anything else: it is the last thing
	// keeping the child's stdout open, so returning it ends a drain that is still
	// reading. A spawned holder would stay behind instead and, on Windows, keep the
	// test binary from being unlinked after the package finished.
	if tt.heldWriteEnd != nil {
		tt.heldWriteEnd.Close()
		tt.heldWriteEnd = nil
	}
	// A copy may still be running (it does not hold this lock), so use TryLock
	// rather than Lock: the cleanup must not wait behind a descriptor it is about
	// to release anyway, and the copy bounds itself and therefore always ends.
	if !tt.stdoutMu.TryLock() {
		return
	}
	defer tt.stdoutMu.Unlock()
	tt.releaseStdoutLocked()
}

// releaseStdoutLocked closes the read end without waiting for more output, so a
// caller that must not block still gives the descriptor back. Whatever the child
// wrote but nobody read is dropped; the text already collected stays readable. The
// caller has to hold stdoutMu.
func (tt *TestCmd) releaseStdoutLocked() {
	if tt.stdoutPipe == nil {
		return
	}
	tt.stdoutPipe.Close()
	tt.stdoutPipe = nil
	tt.stdoutDrained = true
}

// Interrupt asks the child to stop and returns immediately, without running the
// caller's Cleanup hook: the child still has a graceful shutdown to finish, and that
// hook owns resources - a datadir, a socket - the child is still writing to. Kill
// terminates the child outright (SIGKILL on Unix, TerminateProcess on Windows), after
// which it cannot touch anything, so Kill does run the hook. Interrupt is paired with
// ExpectExit, which runs the hook once the child has been reaped; WaitExit does not
// run it, so a caller that pairs Interrupt with WaitExit keeps the hook itself.
func (tt *TestCmd) Interrupt() {
	tt.setErr(tt.cmd.Process.Signal(os.Interrupt))
}

// ExitStatus exposes the process' OS exit code, or -1 when the exit was never
// observed: a reap that gave up leaves Err nil, and reporting 0 there would turn a
// command that failed into a successful one for every caller that reads this. On
// Unix a child killed by a signal also reports -1, while Windows reports the
// termination status its kill used, so -1 is not a portable signal verdict; see
// ExitObserved for telling a -1 that means "no exit" from an observed exit.
func (tt *TestCmd) ExitStatus() int {
	exitErr, ok := tt.errValue().(*exec.ExitError)
	if !ok {
		// Err carries no observed exit: the reap gave up (Err nil), or Err holds the
		// error from a failed Interrupt. Either way only the shared wait can answer,
		// and only its close orders the write of waitErr before this read. Before
		// that close the exit has not been observed, and reporting 0 would turn a
		// command that failed into a successful one.
		select {
		case <-tt.waitDone:
			if tt.waitErr == nil {
				return 0 // the shared wait saw a clean exit
			}
			if exitErr, ok = tt.waitErr.(*exec.ExitError); !ok {
				return -1 // an error with no exit status is still not an observed exit
			}
		default:
			return -1
		}
	}
	if status, isWaitStatus := exitErr.Sys().(syscall.WaitStatus); isWaitStatus {
		return status.ExitStatus()
	}
	return -1
}

// ExitObserved reports whether the child's exit is known. ExitStatus returns -1 both
// for a process that a signal killed and for an exit that no wait has observed yet,
// and this is what tells the two apart: with ExitObserved true, a -1 is the signal's
// verdict rather than a missing answer.
func (tt *TestCmd) ExitObserved() bool {
	if _, ok := tt.errValue().(*exec.ExitError); ok {
		return true
	}
	select {
	case <-tt.waitDone:
		return true
	default:
		return false
	}
}

// StderrText returns any stderr output written so far, up to the logger's cap; what
// exceeded it is dropped, and the returned text says how much. The returned text
// holds all log lines after ExpectExit has returned.
func (tt *TestCmd) StderrText() string {
	return tt.stderr.text()
}

// StdoutText returns everything read from the child's stdout so far, including the
// text the Expect helpers already consumed. Only data still sitting in the pipe -
// written by the child but not read off it yet - is missing: the tee sits under the
// reader, so it records every byte as the reader pulls it, whether the Expect
// helpers or the drain did the reading.
//
// It is a snapshot: it neither drains the pipe nor closes it, and it does not wait
// behind a drain that is in flight, so it may be called while the child is still
// running and the Expect helpers keep working afterwards. WaitExit drains what is
// left as it reaps the child, so reading the complete output means reading after
// WaitExit has returned. A drain that never saw EOF - it stopped on its own bound,
// or a helper timeout released the read end before WaitExit reached the copy - ends
// with the text possibly short, and StdoutText then says so ahead of the text.
//
// It exists because the two streams are not interchangeable. utils.Fatalf writes to
// stdout alone on Windows and to both streams elsewhere, so a message-level
// assertion that only reads stderr fails on one of the two platforms whichever
// stream it picks.
func (tt *TestCmd) StdoutText() string {
	if tt.stdoutLog == nil {
		return ""
	}
	text := tt.stdoutLog.text()
	if tt.stdoutTruncated.Load() {
		return "... (the drain never saw EOF; the text below may be incomplete) ...\n" + text
	}
	return text
}

func (tt *TestCmd) CloseStdin() {
	tt.stdin.Close()
}

func (tt *TestCmd) Kill() {
	tt.cmd.Process.Kill()
	tt.runCleanup()
}

// runCleanup invokes the caller's Cleanup hook at most once, however many paths
// reach it for the same child.
func (tt *TestCmd) runCleanup() {
	if tt.Cleanup != nil {
		tt.cleanupOnce.Do(tt.Cleanup)
	}
}

// killTimeout is the bound the helpers waiting for child output apply before they
// give up on it.
func (tt *TestCmd) killTimeout() time.Duration {
	if tt.KillTimeout > 0 {
		return tt.KillTimeout
	}
	// The historical default.
	return 30 * time.Second
}

// logLimit is how much of each stream the loggers keep for inspection. Background
// writers call it from their own goroutine, so it reads the snapshot Run took
// rather than MaxLogSize itself.
func (tt *TestCmd) logLimit() int {
	if limit := tt.maxLogSize.Load(); limit > 0 {
		return int(limit)
	}
	return defaultMaxLogSize
}

// SetMaxLogSize changes the cap of a child that is already running. It is safe
// from any goroutine, unlike an assignment to MaxLogSize, which the loggers no
// longer read once Run has snapshotted it.
func (tt *TestCmd) SetMaxLogSize(limit int) {
	tt.maxLogSize.Store(int64(limit))
}

func (tt *TestCmd) withKillTimeout(fn func()) {
	// claimed is set by whichever side gets there first, and that is what makes the
	// two exclusive: a timer that fires while the helper is still reading claims the
	// kill and releases the read end, and a helper that returns first claims the
	// completion, after which a late timer does nothing.
	//
	// Stop does not wait for a running callback, so a Load followed by a Store would
	// leave a window in which the helper has already matched and the timer still kills
	// its child and runs the caller's Cleanup hook - os.RemoveAll on a datadir, for
	// one. The CompareAndSwap makes the two sides exclusive instead of merely narrowing
	// that window: whichever arrives first claims it and the other side does nothing.
	// A helper that returns still leaves the few instructions between its return and
	// the Store below, and that residue cannot be closed. No second check is needed
	// before releasing the read end, because a helper that matched has already
	// returned and has therefore already claimed the flag.
	var claimed atomic.Bool
	timeout := time.AfterFunc(tt.killTimeout(), func() { tt.killOnHelperTimeout(&claimed) })
	defer timeout.Stop()
	fn()
	claimed.Store(true)
}

// killOnHelperTimeout is what the timer above runs when the helper gave up on the
// output. It is split out so a test can drive it directly: the timer is stopped as soon
// as the helper returns, so the branch that has to leave an already returned helper
// alone is otherwise unreachable.
func (tt *TestCmd) killOnHelperTimeout(claimed *atomic.Bool) {
	if !claimed.CompareAndSwap(false, true) {
		return
	}
	tt.logf("killing the child process (timeout)")
	tt.Kill()
	// Killing the child does not end a read that a process it spawned keeps alive, so
	// give the read end back as well: the helper then fails with a read error instead
	// of blocking until go test's own timeout.
	tt.stdoutMu.Lock()
	// The helper never matched, so its output may be missing a tail, and this release
	// also keeps WaitExit's copy from reaching the io.Copy that would otherwise mark
	// that: copyStdout returns early once stdoutDrained is set.
	tt.stdoutTruncated.Store(true)
	tt.releaseStdoutLocked()
	tt.stdoutMu.Unlock()
}

// defaultMaxLogSize caps how much of a child's output a testlogger keeps for
// inspection when TestCmd.MaxLogSize is not set: a child that logs for minutes
// would otherwise pin its whole output in memory.
const defaultMaxLogSize = 4 << 20

// testlogger logs all written lines via t.Log and also
// collects them for later inspection.
type testlogger struct {
	t      *testing.T
	mu     sync.Mutex
	buf    bytes.Buffer
	name   string
	stream string
	// maxLimit returns the cap buf is held to; a nil value falls back to
	// defaultMaxLogSize. The cap is read on every write, so a change to
	// TestCmd.MaxLogSize takes effect for everything written after it; the field
	// says which goroutine does the reading and when the assignment is safe.
	maxLimit func() int
	// tDone reports that the test has finished. The stderr copier can still be
	// running then, and a t.Logf from that goroutine panics.
	tDone *atomic.Bool
	// logLines reports each non-empty line to t.Log as it is written. The stdout
	// logger leaves it off: the Expect helpers and StdoutText already surface that
	// text, and echoing a node's console output a second time buries the test log.
	logLines bool
	// dropped counts the bytes not kept because buf was full.
	dropped int
}

func (tl *testlogger) Write(b []byte) (n int, err error) {
	if tl.logLines && (tl.tDone == nil || !tl.tDone.Load()) {
		lines := bytes.Split(b, []byte("\n"))
		for _, line := range lines {
			if len(line) > 0 {
				tl.t.Logf("(%s:%v) %s", tl.stream, tl.name, line)
			}
		}
	}
	tl.mu.Lock()
	limit := defaultMaxLogSize
	if tl.maxLimit != nil {
		if l := tl.maxLimit(); l > 0 {
			limit = l
		}
	}
	firstDrop := false
	if room := limit - tl.buf.Len(); room > 0 {
		if len(b) <= room {
			tl.buf.Write(b)
		} else {
			tl.buf.Write(b[:room])
			if tl.dropped == 0 {
				firstDrop = true
			}
			tl.dropped += len(b) - room
		}
	} else {
		if tl.dropped == 0 {
			firstDrop = true
		}
		tl.dropped += len(b)
	}
	kept, dropped := tl.buf.Len(), tl.dropped
	tl.mu.Unlock()
	// Note the first truncation without failing the test: the callers assert with
	// Contains, so a cap that cuts the tail is worth saying out loud, while making
	// it an error would fail tests whose assertions the kept head still satisfies.
	if firstDrop && (tl.tDone == nil || !tl.tDone.Load()) {
		tl.t.Logf("(%s:%v) output capped at %d bytes kept (%d dropped); raise TestCmd.MaxLogSize to keep more",
			tl.stream, tl.name, kept, dropped)
	}
	return len(b), nil
}

// text returns the collected log, noting up front when output had to be dropped.
func (tl *testlogger) text() string {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	if tl.dropped > 0 {
		return fmt.Sprintf("... (%d bytes dropped after the first %d) ...\n", tl.dropped, tl.buf.Len()) + tl.buf.String()
	}
	return tl.buf.String()
}

// runeTee collects text read through it into buf.
type runeTee struct {
	in interface {
		io.Reader
		io.ByteReader
		io.RuneReader
	}
	buf bytes.Buffer
}

func (rtee *runeTee) Read(b []byte) (n int, err error) {
	n, err = rtee.in.Read(b)
	rtee.buf.Write(b[:n])
	return n, err
}

func (rtee *runeTee) ReadRune() (r rune, size int, err error) {
	r, size, err = rtee.in.ReadRune()
	if err == nil {
		rtee.buf.WriteRune(r)
	}
	return r, size, err
}

func (rtee *runeTee) ReadByte() (b byte, err error) {
	b, err = rtee.in.ReadByte()
	if err == nil {
		rtee.buf.WriteByte(b)
	}
	return b, err
}
