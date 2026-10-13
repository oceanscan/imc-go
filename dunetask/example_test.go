package dunetask_test

import (
	"context"
	"fmt"
	"os"
	"time"

	imc "github.com/oceanscan/imc-go"
	"github.com/oceanscan/imc-go/dunetask"
	"github.com/oceanscan/imc-go/dunetask/dunetest"
)

// A task that watches the depth: one entity with parameters, activation, a
// typed handler and a periodic job. dunetask.Main runs it until SIGTERM,
// with the bus from $DUNE_BUS (or tcp://127.0.0.1:6060).
func ExampleNew() {
	t := dunetask.New("Depth Watcher", dunetask.HonoursActive())
	maxDepth := dunetask.NewParam(t, "Maximum Depth", 5.0, dunetask.Units("m"), dunetask.Min(0))

	dunetask.Consume(t, func(m *imc.EstimatedState) {
		nav, ok := t.EidOf("Navigation")
		if !t.Active() || !ok || m.Header.SrcEnt != nav {
			return
		}
		if float64(m.Depth) > maxDepth.Get() {
			t.State(imc.EntityStateState_FAULT, "too deep")
			t.War("depth %.1f m", m.Depth) // a LogBookEntry
			t.Dispatch(&imc.TextMessage{Text: "too deep"})
		}
	})
	t.Periodic(10*time.Second, func() { t.Debug("still watching") })
	t.OnActivation = func() error {
		t.Inf("watching depth (maximum %.1f m)", maxDepth.Get())
		return nil
	}

	if len(os.Args) > 1 && os.Args[1] == "run" { // (not when the example runs as a test)
		dunetask.Main(dunetask.Options{}, t)
	}
	// Output:
}

// Running a task against the fake DUNE of package dunetest: the same
// handshake, entity services and id translation as DUNE.
func ExampleRun() {
	srv, _ := dunetest.Start(dunetest.Config{Entities: []string{"Navigation"}})
	defer srv.Close()

	t := dunetask.New("Echo")
	dunetask.Consume(t, func(m *imc.EstimatedState) {
		t.Inf("depth %.2f m", m.Depth)
	})
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error)
	go func() {
		done <- dunetask.Run(ctx, dunetask.Options{Bus: srv.URL(), Name: "echo",
			Output: dunetask.NewOutput(os.Stderr, dunetask.LevelError, false)}, t)
	}()

	id, _ := srv.WaitEntity("Echo", 5*time.Second)
	srv.WaitMessage(dunetest.Of[*imc.EntityInfo](), 5*time.Second)
	srv.Send(&imc.EstimatedState{Depth: 2.5}, "Navigation", "")
	m, _ := srv.WaitMessage(dunetest.Of[*imc.LogBookEntry](), 5*time.Second)
	log := m.(*imc.LogBookEntry)
	fmt.Println(log.Type, log.Context, log.Text, int(log.Header.SrcEnt) == id)

	stop()
	fmt.Println(<-done)
	// Output:
	// INFO Echo depth 2.50 m true
	// <nil>
}

// Parameters travel as text, with DUNE's syntax; the text of a value is
// kept as it was set.
func ExampleNewParam() {
	t := dunetask.New("Sampler")
	rate := dunetask.NewParam(t, "Rate", 2.0, dunetask.Units("Hz"), dunetask.Min(0.1), dunetask.Max(50))
	modes := dunetask.NewParam(t, "Modes", []string{"a", "b"})
	enabled := dunetask.NewParam(t, "Enabled", true)

	fmt.Println(t.ParamText("Rate"), t.ParamText("Modes"), t.ParamText("Enabled"))
	fmt.Println(rate.Get(), modes.Get(), enabled.Get())
	fmt.Println(rate.Set(100))
	// Output:
	// 2.0 a, b true
	// 2 [a b] true
	// Rate: value is above maximum
}

// Replying to a request: the reply goes to the requester's system and
// entity.
func ExampleTask_Reply() {
	t := dunetask.New("Pinger")
	dunetask.Consume(t, func(m *imc.DevDataText) {
		if m.Value == "ping" {
			t.Reply(m, &imc.DevDataText{Value: "pong"})
		}
	})
	// Output:
}

// The bus options default to what Supervisors.Process sets.
func ExampleEnvDefaults() {
	os.Setenv("DUNE_BUS", "unix:///run/dune/bus.sock")
	os.Setenv("DUNE_SYSTEM", "0x2b01")
	defer os.Unsetenv("DUNE_BUS")
	defer os.Unsetenv("DUNE_SYSTEM")
	e := dunetask.EnvDefaults()
	fmt.Println(e.Bus, e.System)
	// Output: unix:///run/dune/bus.sock 11009
}
