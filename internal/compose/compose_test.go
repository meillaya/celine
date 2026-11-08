package compose

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Meillaya/celine/internal/execx"
)

func TestRunningAndStartedAt(t *testing.T) {
	runner := &execx.Fake{Func: func(call execx.Call) (execx.Result, error) {
		joined := strings.Join(call.Args, " ")
		switch {
		case strings.Contains(joined, " ps -q"):
			return execx.Result{Stdout: []byte("abc123\n")}, nil
		case strings.Contains(joined, "inspect"):
			return execx.Result{Stdout: []byte("2026-11-08T12:00:00.000000000Z\n")}, nil
		default:
			return execx.Result{}, nil
		}
	}}
	client := New(runner, "/repo/build/compose.yaml")
	running, err := client.Running(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !running {
		t.Fatal("Running = false")
	}
	started, ok, err := client.StartedAt(context.Background(), "kavita")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("StartedAt = false")
	}
	want := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	if !started.Equal(want) {
		t.Fatalf("started = %s, want %s", started, want)
	}
}

func TestRunningEmptyProject(t *testing.T) {
	runner := &execx.Fake{}
	client := New(runner, "/repo/build/compose.yaml")
	running, err := client.Running(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if running {
		t.Fatal("Running = true for an empty project")
	}
}

func TestRestartCommand(t *testing.T) {
	runner := &execx.Fake{}
	client := New(runner, "/repo/build/compose.yaml")
	if err := client.Restart(context.Background(), "kavita"); err != nil {
		t.Fatal(err)
	}
	line := runner.CommandLines()[0]
	if !strings.Contains(line, "restart kavita") {
		t.Fatalf("restart command = %q", line)
	}
}
