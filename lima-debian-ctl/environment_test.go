package main

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

func TestEnvironmentRecords(t *testing.T) {
	for _, sep := range []string{"\r", "\n", "\r\n"} {
		env, err := parseEnvironment(strings.Join([]string{"A=one=two", "EMPTY=", "SPACE= value ", "QUOTE='literal'", ""}, sep))
		if err != nil {
			t.Fatal(err)
		}
		if env["A"] != "one=two" || env["EMPTY"] != "" || env["SPACE"] != " value " || env["QUOTE"] != "'literal'" {
			t.Fatalf("incorrect parsing for delimiter %q", sep)
		}
	}
}
func TestEnvironmentRejectsInvalidWithoutLeakingValues(t *testing.T) {
	for _, text := range []string{"A=secret\nA=secret", "BAD-NAME=secret", "missing-secret", "A=secret\x00", "HOME=secret", "BASH_ENV=secret", "LIMA_DEBIAN_CTL_PROJECT=secret", "A=secret\ncontinued-secret", string([]byte{0xff})} {
		_, err := parseEnvironment(text)
		if err == nil {
			t.Fatalf("accepted invalid record")
		}
		if strings.Contains(err.Error(), "secret") {
			t.Fatal("error exposed record contents")
		}
	}
	if _, err := parseEnvironment(strings.Repeat("a", environmentLimit+1)); err == nil {
		t.Fatal("accepted oversized output")
	}
}
func TestRenderedExportsAreLiteralInBashAndZsh(t *testing.T) {
	value := "  'quotes' \"double\" $HOME `false` $(false); * \\ ! = #  "
	for _, shell := range []string{"bash", "zsh"} {
		if _, err := exec.LookPath(shell); err != nil {
			t.Skipf("%s unavailable", shell)
		}
		script := renderEnvironment(map[string]string{"VALUE": value}, "project-a") + "printf '%s' \"$VALUE\""
		got, err := exec.Command(shell, "-c", script).Output()
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != value {
			t.Fatalf("%s did not preserve the literal value", shell)
		}
	}
}
func TestResolverFailureWithholdsOutput(t *testing.T) {
	r := runnerFunc(func(_ context.Context, c Command) (Result, error) {
		return Result{Stdout: "SECRET=do-not-log", Stderr: "do-not-log", Code: 1}, nil
	})
	_, err := resolveEnvironment(context.Background(), r, Config{Project: "p", Environment: Environment{Resolver: []string{"resolver"}, TimeoutSeconds: 1}})
	if err == nil || strings.Contains(err.Error(), "do-not-log") {
		t.Fatal("resolver failure leaked output or succeeded")
	}
}

type runnerFunc func(context.Context, Command) (Result, error)

func (f runnerFunc) Run(ctx context.Context, c Command) (Result, error) { return f(ctx, c) }
