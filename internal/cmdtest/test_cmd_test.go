// Copyright 2026 The go-ethereum Authors
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
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docker/docker/pkg/reexec"
)

func init() {
	// The child writes a line, then more than a pipe buffer's worth of bytes, and
	// exits on its own. A test that consumed only the first line leaves the rest for
	// the drain: waiting for this child before reading it would deadlock.
	reexec.Register("cmdtest-bulk-exit-test", func() {
		fmt.Println("bulk")
		buf := bytes.Repeat([]byte("x"), 64*1024)
		for i := 0; i < 4; i++ {
			os.Stdout.Write(buf)
		}
		os.Exit(0)
	})
	// The child bursts on stderr in several writes and exits. exec.Cmd runs a
	// copier for a Stderr that is not an *os.File, so this child puts a background
	// writer on the logger cap while the test's own goroutine is still setting it.
	reexec.Register("cmdtest-stderr-bulk-test", func() {
		buf := bytes.Repeat([]byte("e"), 4*1024)
		for i := 0; i < 8; i++ {
			os.Stderr.Write(buf)
			time.Sleep(10 * time.Millisecond)
		}
		os.Exit(0)
	})
	// The child exits long after reapTimeout but well within KillTimeout, so a
	// WaitExit that gave up early would report its failed exit as a success.
	reexec.Register("cmdtest-slow-exit-test", func() {
		time.Sleep(3 * time.Second)
		os.Exit(1)
	})
	// The child writes one line and exits, leaving its stdout to whoever else holds
	// the write end. Paired with a TestCmd that keeps its own copy, that is the shape
	// the drain cannot wait out: the child is gone, yet the pipe stays open until the
	// last holder lets go of it.
	reexec.Register("cmdtest-exit-test", func() {
		fmt.Println("child wrote its line and exited")
		os.Exit(0)
	})
	// The child exits without writing anything, so draining its stdout cannot report
	// unmatched text.
	reexec.Register("cmdtest-silent-test", func() {
		os.Exit(0)
	})
	// The child writes, pauses, writes again and then stays alive: it lets a test
	// take a snapshot of a running child's stdout and still expect more text after.
	reexec.Register("cmdtest-chatty-test", func() {
		fmt.Println("line 1")
		time.Sleep(300 * time.Millisecond)
		fmt.Println("line 2")
		time.Sleep(time.Hour)
		os.Exit(0)
	})
	// The holder outlives the spawner that started it and keeps the descriptor it
	// was handed. Only a holder of stderr keeps exec.Cmd's copier running, which is
	// what the cleanup reap has to be bounded against, so it must outlive
	// reapTimeout by a margin wide enough that the bound really is what ends the
	// wait: 6s against a 2s bound leaves 4s of slack. The cost is that the test
	// binary's image stays mapped for that whole time after the test is over, so a
	// Windows run has to confirm go test can still rebuild the binary.
	reexec.Register("cmdtest-stderr-holder", func() {
		time.Sleep(6 * time.Second)
		os.Exit(0)
	})
	// The spawner hands its stdout and stderr to the holder and exits, leaving the
	// holder as the only thing keeping those pipes open. It prints its line after
	// Start returns, so a test that reads that line knows the descriptors were
	// inherited instead of sleeping to guess.
	//
	// The path comes from os.Executable rather than reexec.Self: on Windows Self is
	// naiveSelf, which resolves the reexec name this process was started under
	// (os.Args[0]) into a path that does not exist, so a spawner one level below the
	// test binary could not start the holder at all.
	reexec.Register("cmdtest-spawner", func() {
		self, err := os.Executable()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		c := &exec.Cmd{
			Path:   self,
			Args:   []string{"cmdtest-stderr-holder"},
			Stdout: os.Stdout,
			Stderr: os.Stderr,
		}
		if err := c.Start(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("holder started")
		os.Exit(0)
	})
}

func TestMain(m *testing.M) {
	if reexec.Init() {
		return
	}
	os.Exit(m.Run())
}

// TestStdoutTextIsNonDestructive pins that reading a running child's stdout is a
// snapshot: it returns immediately, keeps the Expect helpers working, and neither
// reaps the child nor closes the read end behind the test's back.
func TestStdoutTextIsNonDestructive(t *testing.T) {
	cmd := NewTestCmd(t, nil)
	cmd.KillTimeout = 2 * time.Second
	cmd.Run("cmdtest-chatty-test")

	cmd.Expect("\nline 1\n")

	start := time.Now()
	got := cmd.StdoutText()
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("StdoutText took %v while the child was running, want a snapshot", elapsed)
	}
	if !strings.Contains(got, "line 1") {
		t.Fatalf("StdoutText() = %q, want the text the Expect helper already consumed", got)
	}
	// The pipe must still be usable: this is what a drain here would have broken.
	cmd.Expect("line 2\n")

	cmd.Kill()
	cmd.WaitExit()
}

// TestStdoutTextMarksADrainThatNeverSawEOF pins that a drain which could not reach
// EOF says so: the parent keeps the write end, so the copy stops on its own bound
// even though the child is gone, and the text the child wrote before exiting stays
// readable underneath the note.
func TestStdoutTextMarksADrainThatNeverSawEOF(t *testing.T) {
	cmd := NewTestCmd(t, nil)
	cmd.KillTimeout = 500 * time.Millisecond
	cmd.holdWriteEnd = true
	cmd.Run("cmdtest-exit-test") // writes one line, exits 0

	cmd.WaitExit()
	got := cmd.StdoutText()
	if !strings.Contains(got, "never saw EOF") {
		t.Fatalf("StdoutText() = %q, want the drain to say it never saw EOF", got)
	}
	if !strings.Contains(got, "child wrote its line and exited") {
		t.Fatalf("StdoutText() = %q, want the line the child wrote under the note", got)
	}
}

// TestStdoutTextIsCompleteAfterACleanDrain is the counterpart: a child that exits
// with its stdout closed lets the drain reach EOF, so the text carries no note.
func TestStdoutTextIsCompleteAfterACleanDrain(t *testing.T) {
	cmd := NewTestCmd(t, nil)
	cmd.KillTimeout = 5 * time.Second
	cmd.Run("cmdtest-exit-test") // writes one line, exits 0

	cmd.WaitExit()
	got := cmd.StdoutText()
	if strings.Contains(got, "never saw EOF") {
		t.Fatalf("StdoutText() = %q, want no note after a drain that saw EOF", got)
	}
	if !strings.Contains(got, "child wrote its line and exited") {
		t.Fatalf("StdoutText() = %q, want the line the child wrote", got)
	}
}

// TestWaitExitDrainsStdoutHeldOpenByAnotherHolder pins the bound on the drain when
// the child has exited but the write end is still held. Nothing here can close that
// pipe, so only the drain's own bound ends the read; without it WaitExit would hold
// stdoutMu and every later StdoutText would block behind it.
//
// The holder is the parent's own copy of the write end rather than a process the
// child spawned. Both keep the pipe open past the child, which is all the drain can
// tell, and the parent gives its copy back with the test, so nothing outlives the
// test binary - on Windows a running image cannot be unlinked, which would turn a
// passing package into a failing one.
func TestWaitExitDrainsStdoutHeldOpenByAnotherHolder(t *testing.T) {
	cmd := NewTestCmd(t, nil)
	cmd.KillTimeout = 500 * time.Millisecond
	cmd.holdWriteEnd = true
	cmd.Run("cmdtest-exit-test")

	waited := make(chan struct{})
	go func() {
		cmd.WaitExit()
		close(waited)
	}()
	select {
	case <-waited:
	case <-time.After(10 * time.Second):
		t.Fatal("WaitExit blocked on a stdout that is still held open")
	}
	if cmd.Err != nil {
		t.Fatalf("child exited with %v, want a clean exit", cmd.Err)
	}
	// The text the child wrote before exiting is still readable: the bound drops
	// what was still in flight, not what had already been read.
	if got := cmd.StdoutText(); !strings.Contains(got, "child wrote its line and exited") {
		t.Fatalf("StdoutText() = %q, want the line the child wrote", got)
	}
}

// TestWaitExitIsSafeToCallConcurrently pins that a second WaitExit shares the one drain
// instead of starting a rival reader. The pipe is held open, so the first drain is
// still in flight when the second call arrives; two copies would read the same
// bufio.Reader from two goroutines at once, which the race detector reports, and the
// text the child wrote has to survive the two of them.
func TestWaitExitIsSafeToCallConcurrently(t *testing.T) {
	cmd := NewTestCmd(t, nil)
	cmd.KillTimeout = 500 * time.Millisecond
	cmd.holdWriteEnd = true
	cmd.Run("cmdtest-exit-test") // writes one line, exits 0

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd.WaitExit()
		}()
	}
	wg.Wait()
	if got := cmd.StdoutText(); !strings.Contains(got, "child wrote its line and exited") {
		t.Fatalf("StdoutText() = %q, want the line the child wrote", got)
	}
}

// TestWaitExitDoesNotKillAReapedChild pins that a drain timeout no longer reaches
// for a child WaitExit has already reaped. The user Cleanup hook is the observable:
// draining cannot end a pipe that outlived the child, so nothing should treat the
// timeout as a reason to kill and run the hook.
func TestWaitExitDoesNotKillAReapedChild(t *testing.T) {
	cmd := NewTestCmd(t, nil)
	cmd.KillTimeout = 500 * time.Millisecond
	cmd.holdWriteEnd = true
	cleanups := 0
	cmd.Cleanup = func() { cleanups++ }
	cmd.Run("cmdtest-exit-test")

	cmd.WaitExit()
	if cmd.Err != nil {
		t.Fatalf("child exited with %v, want a clean exit", cmd.Err)
	}
	if cleanups != 0 {
		t.Fatalf("cleanup ran %d times, want 0: the drain killed an already reaped child", cleanups)
	}
}

// TestCleanupDoesNotBlockOnAGrandchildHoldingStderr pins the bound on the reap the
// test cleanup performs. The holder outlives reapTimeout, so a passing test
// proves the reap gave up instead of waiting for it; without the bound the package
// would hang here until go test's own timeout.
func TestCleanupDoesNotBlockOnAGrandchildHoldingStderr(t *testing.T) {
	// The holder sleeps 6s; keep that margin from silently shrinking to the point
	// where the reap bound is never exercised.
	if 6*time.Second <= reapTimeout {
		t.Fatalf("the holder sleeps 6s, which no longer outlives reapTimeout (%v)", reapTimeout)
	}
	start := time.Now()
	// Registered before Run, so it runs after releaseStdout (cleanups run last in,
	// first out) and can time the reap it waited for.
	t.Cleanup(func() {
		elapsed := time.Since(start)
		// Both bounds matter: returning immediately would mean the holder never
		// inherited stderr, so the bound was never exercised and this test would
		// pass without testing anything.
		if elapsed < reapTimeout/2 {
			t.Errorf("cleanup returned in %v; the reap bound was never exercised", elapsed)
		}
		// The upper bound has to stay below the holder's own lifetime: at the
		// holder's 6s an unbounded reap would return close enough to a fixed 5s
		// bound that a loaded machine could pass without the bound having been
		// exercised. The lower bound above proves the bound was reached at all.
		if elapsed > reapTimeout+2*time.Second {
			t.Errorf("cleanup waited %v for a holder outliving the reap bound; the reap is unbounded", elapsed)
		}
	})
	cmd := NewTestCmd(t, nil)
	cmd.KillTimeout = time.Second
	cmd.Run("cmdtest-spawner")
	// The spawner prints this after its Start returned, so reading it means the
	// holder already owns the descriptors the reap has to wait on.
	cmd.Expect("\nholder started\n")
}

// TestExpectExitRunsCleanupOnceAndDoesNotHang pins two bounds at once: the read end
// is given back when a helper times out, so ExpectExit cannot block on a pipe the
// parent still holds, and the user Cleanup hook runs exactly once even though both
// the timeout and ExpectExit reach it.
func TestExpectExitRunsCleanupOnceAndDoesNotHang(t *testing.T) {
	cmd := NewTestCmd(t, nil)
	cmd.KillTimeout = 300 * time.Millisecond
	cmd.holdWriteEnd = true
	cleanups := 0
	cmd.Cleanup = func() { cleanups++ }
	cmd.Run("cmdtest-silent-test")

	done := make(chan struct{})
	go func() {
		defer close(done)
		cmd.ExpectExit()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("ExpectExit blocked on a read end the timeout should have given back")
	}
	if cleanups != 1 {
		t.Fatalf("cleanup ran %d times, want 1", cleanups)
	}
}

// TestWithKillTimeoutEndsABlockedRead pins the second lever of the helper timeout:
// killing the child cannot end a read on a pipe another holder keeps open, so the
// read end is given back as well.
func TestWithKillTimeoutEndsABlockedRead(t *testing.T) {
	cmd := NewTestCmd(t, nil)
	cmd.KillTimeout = 300 * time.Millisecond
	cmd.holdWriteEnd = true
	// The child is gone, but the parent still holds the write end, so the read below
	// never sees EOF on its own.
	cmd.Run("cmdtest-exit-test")

	start := time.Now()
	cmd.withKillTimeout(func() {
		_, _ = io.ReadAll(cmd.stdout)
	})
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("withKillTimeout did not bound the read: %v", elapsed)
	}
	// The read end is gone, so a later read fails instead of blocking again.
	if _, err := cmd.stdout.Read(make([]byte, 1)); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("read after the timeout = %v, want os.ErrClosed", err)
	}
}

// TestWithKillTimeoutDoesNotKillAFinishedHelper pins the half of the claim the timeout
// makes that is reachable: a helper that returned inside its deadline leaves the child
// alone, so the timer must not kill it or run the caller's Cleanup hook. The callback is
// driven directly, because withKillTimeout stops its timer as soon as the helper
// returns - that is what makes the branch unreachable through it. The interleaving the
// claim also has to rule out, a timer that fires as the helper returns, cannot be built
// reliably and stays pinned by the CompareAndSwap alone.
func TestWithKillTimeoutDoesNotKillAFinishedHelper(t *testing.T) {
	cmd := NewTestCmd(t, nil)
	cleanups := 0
	cmd.Cleanup = func() { cleanups++ }
	cmd.Run("cmdtest-chatty-test") // stays alive for an hour

	var claimed atomic.Bool
	claimed.Store(true) // the helper returned and claimed the completion
	cmd.killOnHelperTimeout(&claimed)
	if cleanups != 0 {
		t.Fatalf("cleanup ran %d times for a helper that had already returned, want 0", cleanups)
	}
	// Kill is the positive control: the child is still alive, and the hook this test
	// asserts on really does run when a path reaches it.
	cmd.Kill()
	if cleanups != 1 {
		t.Fatalf("cleanup ran %d times after Kill, want 1", cleanups)
	}
}

// TestWaitExitDrainsUnconsumedBulkStdout pins the two halves of the deadlock fix: a
// child that filled the pipe can only exit once somebody reads it, and the drain
// doing that reading has to finish before StdoutText can be complete.
func TestWaitExitDrainsUnconsumedBulkStdout(t *testing.T) {
	const bulk = 256 * 1024
	cmd := NewTestCmd(t, nil)
	cmd.KillTimeout = 5 * time.Second
	cmd.Run("cmdtest-bulk-exit-test")
	cmd.Expect("\nbulk\n") // consume the first line only

	start := time.Now()
	cmd.WaitExit()
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("WaitExit took %v, want a bounded reap and drain", elapsed)
	}
	if cmd.Err != nil {
		t.Fatalf("child exited with %v, want a clean exit", cmd.Err)
	}
	if want := len("bulk\n") + bulk; len(cmd.StdoutText()) != want {
		t.Fatalf("StdoutText() = %d bytes, want %d: the drain did not collect the whole stdout",
			len(cmd.StdoutText()), want)
	}
}

// TestDrainDoesNotFollowKillTimeout pins that the drain has its own bound: with the
// 90s KillTimeout that cmd/XDC/accountcmd_test.go sets, a pipe nobody can close
// must not hold WaitExit anywhere near that long.
func TestDrainDoesNotFollowKillTimeout(t *testing.T) {
	cmd := NewTestCmd(t, nil)
	cmd.KillTimeout = 90 * time.Second
	cmd.holdWriteEnd = true
	cmd.Run("cmdtest-exit-test")

	start := time.Now()
	cmd.WaitExit()
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("WaitExit took %v with KillTimeout=90s; the drain is following a knob it must not", elapsed)
	}
	if got := cmd.StdoutText(); !strings.Contains(got, "child wrote its line and exited") {
		t.Fatalf("StdoutText() = %q, want the line the child wrote", got)
	}
}

// TestWaitExitReapsAChildSlowerThanTheCleanupBound pins that the reap WaitExit
// performs follows the test's own budget. A child taking longer than the cleanup
// bound must still be waited for: giving up leaves Err nil, and ExitStatus would
// then report the failed exit as 0.
func TestWaitExitReapsAChildSlowerThanTheCleanupBound(t *testing.T) {
	if 3*time.Second <= reapTimeout {
		t.Fatalf("this child exits after 3s, which no longer outlives reapTimeout (%v)", reapTimeout)
	}
	cmd := NewTestCmd(t, nil)
	cmd.KillTimeout = 30 * time.Second
	cmd.Run("cmdtest-slow-exit-test")

	start := time.Now()
	cmd.WaitExit()
	if elapsed := time.Since(start); elapsed < 3*time.Second {
		t.Fatalf("WaitExit returned in %v; it gave up before the child exited", elapsed)
	}
	if status := cmd.ExitStatus(); status != 1 {
		t.Fatalf("ExitStatus() = %d, want 1: a reap that gave up reports the exit as unobserved", status)
	}
}

// TestReapBudgetIsNotSticky pins that a caller which gives up does not spend the wait
// for the others: a reap with the cleanup's short budget must not hide the exit from a
// later caller that arrives with the test's own budget.
func TestReapBudgetIsNotSticky(t *testing.T) {
	cmd := NewTestCmd(t, nil)
	cmd.KillTimeout = 30 * time.Second
	cmd.Run("cmdtest-slow-exit-test") // exits 1 after 3s

	// The cleanup's budget first, and it gives up while the child is still running.
	if err := cmd.reap(500 * time.Millisecond); err != nil {
		t.Fatalf("short reap = %v; want the reap to give up", err)
	}
	select {
	case <-cmd.waitDone:
		t.Fatal("the short reap observed an exit; it did not give up")
	default:
	}
	// The longer budget has to wait the rest out and still observe the exit.
	start := time.Now()
	err := cmd.reap(30 * time.Second)
	if elapsed := time.Since(start); elapsed < 2*time.Second {
		t.Fatalf("second reap returned in %v without waiting for the child", elapsed)
	}
	if err == nil {
		t.Fatal("second reap = nil; want the exit the first reap gave up on")
	}
	if _, ok := err.(*exec.ExitError); !ok {
		t.Fatalf("second reap = %T, want an *exec.ExitError from the child's exit 1", err)
	}
	// A reap that gave up must not turn the child's failed exit into a successful
	// one once the background wait has completed.
	if status := cmd.ExitStatus(); status != 1 {
		t.Fatalf("ExitStatus() = %d, want 1", status)
	}
}

// TestExitStatusUnobservedIsMinusOne pins that a reap which gave up cannot answer for
// an exit it never saw: the child stays alive past the test's budget, so no wait can
// have observed anything by the time the status is read.
func TestExitStatusUnobservedIsMinusOne(t *testing.T) {
	cmd := NewTestCmd(t, nil)
	cmd.KillTimeout = 300 * time.Millisecond
	cmd.Run("cmdtest-chatty-test") // stays alive for an hour

	cmd.WaitExit() // the reap gives up, then the drain gets its own bound
	if status := cmd.ExitStatus(); status != -1 {
		t.Fatalf("ExitStatus() = %d, want -1: no wait has observed an exit", status)
	}
	if cmd.Err != nil {
		t.Fatalf("Err = %v, want nil for a reap that gave up", cmd.Err)
	}
}

// TestExitObservedTellsAMissingExitFromASignalledOne pins what the exit code can
// mean: ExitStatus answers -1 for an exit nobody has observed, and Unix also
// answers -1 for a child a signal ended while Windows reports the termination
// status its kill used. ExitObserved tells a missing answer from an observed exit,
// which is the part that has to hold on every platform.
func TestExitObservedTellsAMissingExitFromASignalledOne(t *testing.T) {
	t.Run("unobserved", func(t *testing.T) {
		cmd := NewTestCmd(t, nil)
		cmd.KillTimeout = 300 * time.Millisecond
		cmd.Run("cmdtest-chatty-test")
		cmd.WaitExit() // gives up; the child is still running
		if status := cmd.ExitStatus(); status != -1 {
			t.Fatalf("ExitStatus() = %d, want -1", status)
		}
		if cmd.ExitObserved() {
			t.Fatal("ExitObserved() = true for a child no wait has reaped")
		}
	})
	t.Run("signalled", func(t *testing.T) {
		cmd := NewTestCmd(t, nil)
		cmd.Run("cmdtest-chatty-test")
		cmd.Kill()
		cmd.WaitExit()
		// A kill reports -1 on Unix and the termination status Windows uses for
		// TerminateProcess, so only "observed and not a success" is portable.
		if status := cmd.ExitStatus(); status == 0 {
			t.Fatalf("ExitStatus() = %d, want a non-zero status for a killed child", status)
		}
		if !cmd.ExitObserved() {
			t.Fatal("ExitObserved() = false for a child the reap observed")
		}
	})
}

// TestLogLimitDropsTailAndNotesIt pins the cap the loggers apply: the head is kept,
// the tail is dropped, and text() says how much went missing.
func TestLogLimitDropsTailAndNotesIt(t *testing.T) {
	tl := &testlogger{t: t, stream: "stderr", maxLimit: func() int { return 8 }}
	if n, err := tl.Write([]byte("0123456789ABCDEF")); n != 16 || err != nil {
		t.Fatalf("Write = %d, %v", n, err)
	}
	got := tl.text()
	if !strings.HasPrefix(got, "... (8 bytes dropped after the first 8) ...") {
		t.Fatalf("text() = %q, want the dropped bytes noted up front", got)
	}
	if !strings.HasSuffix(got, "01234567") {
		t.Fatalf("text() = %q, want the head kept and the tail dropped", got)
	}
}

// TestSetMaxLogSizeAppliesToARunningChild pins that a cap installed after Run
// bounds both loggers. The stderr case is the one the race detector watches: exec.Cmd
// writes that logger from a copier goroutine while the test installs the cap, so the
// snapshot Run takes is what keeps the two apart.
func TestSetMaxLogSizeAppliesToARunningChild(t *testing.T) {
	for _, tc := range []struct {
		name  string
		child string
		text  func(*TestCmd) string
	}{
		{"stdout", "cmdtest-bulk-exit-test", (*TestCmd).StdoutText},
		{"stderr", "cmdtest-stderr-bulk-test", (*TestCmd).StderrText},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := NewTestCmd(t, nil)
			cmd.KillTimeout = 5 * time.Second
			cmd.Run(tc.child)
			cmd.SetMaxLogSize(16) // installed after Run: must still apply

			cmd.WaitExit()
			if got := tc.text(cmd); !strings.Contains(got, "bytes dropped") {
				t.Fatalf("%s = %q, want the cap installed after Run to have applied", tc.name, got)
			}
		})
	}
}

// TestReleaseStdoutClosesStdin pins that the cleanup gives the stdin write end back.
// exec.Cmd.Wait would close it on its own, but the cleanup's reap can give up before
// Wait completes, and then the descriptor would outlive the test.
func TestReleaseStdoutClosesStdin(t *testing.T) {
	cmd := NewTestCmd(t, nil)
	cmd.Run("cmdtest-chatty-test")

	cmd.releaseStdout()
	if _, err := cmd.stdin.Write([]byte("x")); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("write to stdin after releaseStdout = %v, want os.ErrClosed", err)
	}
}

// TestInterruptDoesNotRunCleanup pins the difference between Interrupt and Kill: the
// child still has a graceful shutdown to finish, so the caller's Cleanup hook - which
// owns the datadir for the tests in cmd/XDC - waits for ExpectExit or WaitExit.
func TestInterruptDoesNotRunCleanup(t *testing.T) {
	cmd := NewTestCmd(t, nil)
	cleanups := 0
	cmd.Cleanup = func() { cleanups++ }
	cmd.Run("cmdtest-chatty-test")

	cmd.Interrupt()
	if cleanups != 0 {
		t.Fatalf("cleanup ran %d times on Interrupt, want 0", cleanups)
	}
	cmd.Kill()
	if cleanups != 1 {
		t.Fatalf("cleanup ran %d times after Kill, want 1", cleanups)
	}
}
