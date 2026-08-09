package tests

import (
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestSIGTERMShutsDownCleanly pins the difference between handling SIGTERM and
// not handling it.
//
// A Go process with no handler for SIGTERM is terminated *by the signal*: it
// never returns from main, and its parent sees a process killed by signal 15
// rather than one that exited. Everything in flight dies with it. That is what
// this service did until it grew a shutdown path, and publish-db.sh restarts
// the service on every single release — so this is a routine event, not an
// exceptional one.
//
// The assertion is the exit status, because that is the part that cannot be
// faked: only a process that returned from main normally exits 0.
func TestSIGTERMShutsDownCleanly(t *testing.T) {
	dir := stageArtifact(t, nil)
	in, err := startService(t, dir)
	if err != nil {
		t.Fatalf("service did not start: %v\n--- log ---\n%s", err, in.log())
	}

	// shutdownTimeout is 10s; allow for that plus slack, so a genuine hang is
	// reported as a hang rather than as a timeout of this test.
	waitErr := in.signal(t, syscall.SIGTERM, 20*time.Second)

	if waitErr != nil {
		var exitErr *exec.ExitError
		if ok := asExitError(waitErr, &exitErr); ok {
			status, isUnix := exitErr.Sys().(syscall.WaitStatus)
			switch {
			case isUnix && status.Signaled():
				t.Fatalf("the service was killed by %v instead of shutting down: "+
					"SIGTERM is not being handled, and every in-flight request dies "+
					"with the process on each release restart", status.Signal())
			default:
				t.Fatalf("the service exited %d on SIGTERM, want 0\n--- log ---\n%s",
					exitErr.ExitCode(), in.log())
			}
		}
		t.Fatalf("waiting for the service: %v", waitErr)
	}

	// The log is the secondary check: it proves the shutdown path ran rather
	// than the process merely happening to end.
	log := in.log()
	for _, want := range []string{"Shutting down", "Stopped"} {
		if !strings.Contains(log, want) {
			t.Errorf("the service exited 0 but its log never says %q — is the "+
				"shutdown path actually running?\n--- log ---\n%s", want, log)
		}
	}
}

// TestShutdownIsPromptWhenIdle guards the other direction. A shutdown that
// waits out its full timeout on an idle service would add ten seconds of
// downtime to every release, which is worse than what it replaced.
func TestShutdownIsPromptWhenIdle(t *testing.T) {
	dir := stageArtifact(t, nil)
	in, err := startService(t, dir)
	if err != nil {
		t.Fatalf("service did not start: %v\n--- log ---\n%s", err, in.log())
	}

	start := time.Now()
	if waitErr := in.signal(t, syscall.SIGTERM, 20*time.Second); waitErr != nil {
		t.Fatalf("unclean exit: %v", waitErr)
	}
	took := time.Since(start)

	// Nothing is in flight, so this should be near-instant. Two seconds is
	// generous enough not to flake on a loaded CI box and still far below the
	// 10s shutdown timeout, which is what a regression would hit.
	if took > 2*time.Second {
		t.Errorf("an idle shutdown took %s; it should be immediate, and this "+
			"delay is added to every release restart", took.Round(time.Millisecond))
	}
}

// asExitError is errors.As, spelled out to keep the import list of this file
// to the things it is actually testing.
func asExitError(err error, target **exec.ExitError) bool {
	if e, ok := err.(*exec.ExitError); ok {
		*target = e
		return true
	}
	return false
}
