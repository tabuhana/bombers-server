package console

import (
	"bufio"
	"bytes"
	"io"
	"strings"
	"testing"
	"time"
)

func consoleReading(in io.Reader) (*Console, *bytes.Buffer) {
	out := &bytes.Buffer{}
	return &Console{out: out, scanner: bufio.NewScanner(in)}, out
}

// The point of the whole change: the time an operator takes to type is not on the clock of
// the work that follows. The answer arrives 300ms after the question, and the context
// handed back has to have been started after that, not when the question was asked.
func TestConfirmStartsTheWorkClockAfterTheAnswer(t *testing.T) {
	pr, pw := io.Pipe()
	c, _ := consoleReading(pr)

	answered := make(chan time.Time, 1)
	go func() {
		time.Sleep(300 * time.Millisecond)
		answered <- time.Now()
		_, _ = io.WriteString(pw, "alice\n")
	}()

	work, cancel, ok := c.confirm("type it: ", "alice")
	defer cancel()
	if !ok || work == nil {
		t.Fatalf("a right answer was refused (ok=%v)", ok)
	}
	deadline, has := work.Deadline()
	if !has {
		t.Fatal("the work context has no deadline at all")
	}
	if at := <-answered; deadline.Before(at.Add(commandTimeout)) {
		t.Errorf("deadline %v is before the answer plus %v (answer at %v): the wait was on the clock", deadline, commandTimeout, at)
	}
	if work.Err() != nil {
		t.Errorf("the work context is already done: %v", work.Err())
	}
}

// Only the exact name is a yes (around whitespace); anything else is a no, and a no hands
// back nothing to run under, with a cancel that is safe to call or defer.
func TestConfirmAcceptsOnlyTheExactName(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		want        bool
	}{
		{"the name", "alice\n", true},
		{"the name with no newline at the end of the input", "alice", true},
		{"the name between spaces and tabs", "\t alice \t\n", true},
		{"another name", "bob\n", false},
		{"the name in the wrong case", "Alice\n", false},
		{"a prefix of the name", "alic\n", false},
		{"the name and more", "alice please\n", false},
		{"the name in quotes", "\"alice\"\n", false},
		{"an empty line", "\n", false},
		{"the input ending", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := consoleReading(strings.NewReader(tc.input))
			work, cancel, ok := c.confirm("type it: ", "alice")
			cancel()
			cancel() // twice, as a defer after an explicit call would
			if ok != tc.want {
				t.Fatalf("ok = %v, want %v", ok, tc.want)
			}
			if ok && work == nil {
				t.Error("a yes handed back no context")
			}
			if !ok && work != nil {
				t.Error("a no handed back a context")
			}
		})
	}
}

// With no input to ask on, the answer is no and the operator is told why: an irreversible
// action must never proceed because nobody could be asked.
func TestConfirmWithNoScannerRefusesAndSaysSo(t *testing.T) {
	out := &bytes.Buffer{}
	c := &Console{out: out}
	work, cancel, ok := c.confirm("type it: ", "alice")
	defer cancel()
	if ok || work != nil {
		t.Errorf("ok=%v work=%v: want a refusal", ok, work)
	}
	if !strings.Contains(out.String(), "refusing") || strings.Contains(out.String(), "type it") {
		t.Errorf("output = %q: want the refusal and no prompt", out.String())
	}
}
