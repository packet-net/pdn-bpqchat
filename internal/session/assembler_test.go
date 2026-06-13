package session

import (
	"strings"
	"testing"
)

func TestAssemblerSplitsAndReassembles(t *testing.T) {
	a := NewLineAssembler(0)
	// A line split across two feeds.
	if got := a.Feed([]byte("hel")); len(got) != 0 {
		t.Fatalf("partial line emitted: %v", got)
	}
	got := a.Feed([]byte("lo\rworld\r"))
	if len(got) != 2 || got[0] != "hello" || got[1] != "world" {
		t.Fatalf("lines = %v, want [hello world]", got)
	}
	if a.Pending() {
		t.Fatal("nothing should be pending")
	}
}

func TestAssemblerTerminators(t *testing.T) {
	a := NewLineAssembler(0)
	// CRLF collapses to one terminator; bare LF works; empty line preserved.
	got := a.Feed([]byte("a\r\nb\n\nc\r"))
	want := []string{"a", "b", "", "c"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestAssemblerCapsOverlongLine(t *testing.T) {
	a := NewLineAssembler(4)
	got := a.Feed([]byte("abcdefgh\rok\r"))
	if len(got) != 2 || got[0] != "abcd" || got[1] != "ok" {
		t.Fatalf("got %v, want [abcd ok] (overflow truncated, next line intact)", got)
	}
}

func TestSanitiseStripsControls(t *testing.T) {
	// \x01 and \x07 (BEL) are dropped; \t becomes a space.
	if got := sanitise("a\x01b\tc\x07d"); got != "ab cd" {
		t.Fatalf("sanitise = %q, want %q", got, "ab cd")
	}
}
