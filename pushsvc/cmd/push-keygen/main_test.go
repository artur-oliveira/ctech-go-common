package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunPrintsCommandsForTheServiceAndWritesNothing(t *testing.T) {
	var out bytes.Buffer
	if err := run(&out, "poker", "prod", true); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		"/ctech-poker/prod/push/vapid-private-key-previous", "--name /ctech-poker/prod/push/vapid-private-key ",
		"--name /ctech-poker/prod/push/vapid-public-key ", "Nothing has been written",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
}

func TestRunRefusesMissingOrUnsafeNames(t *testing.T) {
	for _, c := range [][2]string{{"", "prod"}, {"poker", ""}, {"po ker", "prod"}, {"poker", "pr'od"}} {
		if err := run(&bytes.Buffer{}, c[0], c[1], false); err == nil {
			t.Errorf("run(%q, %q) accepted", c[0], c[1])
		}
	}
}
