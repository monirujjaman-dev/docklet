package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunVersion(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"version"}, &out, &errb); code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errb.String())
	}
	if !strings.HasPrefix(out.String(), "docklet ") {
		t.Fatalf("unexpected output %q", out.String())
	}
}

func TestRunNoArgs(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run(nil, &out, &errb); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
}

func TestRunUnknown(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"bogus"}, &out, &errb); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "unknown command") {
		t.Fatalf("stderr = %q", errb.String())
	}
}
