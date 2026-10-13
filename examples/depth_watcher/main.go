// Command depth_watcher is a DUNE task in Go that watches the vehicle's
// depth (a port of DUNE's examples/python/depth_watcher.py).
//
// While active, it reads EstimatedState from Navigation and, when the depth
// goes over "Maximum Depth", reports its entity in FAULT, logs a warning (a
// LogBookEntry, shown in Neptus and in the vehicle's log) and sends a
// TextMessage, at most once every "Warning Interval" seconds.
//
// Run a DUNE with a DistributedBus server, for example:
//
//	build/dune -c testing/distbus-main -p Simulation      # server on port 6060
//	depth_watcher --bus tcp://127.0.0.1:6060
//
// then, in Neptus, find the entity "Depth Watcher": set its parameters and
// activate it (or send SetEntityParameters{name="Depth Watcher",
// Active=true}).
//
// DUNE can also start it and keep it running (Supervisors.Process): it then
// runs without arguments, with the bus URL, the system and its task name
// in DUNE_BUS, DUNE_SYSTEM and DUNE_TASK_NAME.
package main

import (
	"flag"
	"fmt"
	"strings"
	"time"

	imc "github.com/oceanscan/imc-go"
	"github.com/oceanscan/imc-go/dunetask"
)

func main() {
	bus := flag.String("bus", "", "tcp://host:port or unix:///path of the DistributedBus server "+
		"(default: $DUNE_BUS or "+dunetask.DefaultBus+")")
	flag.Parse()

	t := dunetask.New("Depth Watcher", dunetask.HonoursActive())
	maxDepth := dunetask.NewParam(t, "Maximum Depth", 5.0, dunetask.Units("m"), dunetask.Min(0),
		dunetask.Desc("Depth above which the watcher complains"))
	interval := dunetask.NewParam(t, "Warning Interval", 10.0, dunetask.Units("s"), dunetask.Min(0),
		dunetask.Desc("Minimum time between two warnings"))
	source := dunetask.NewParam(t, "Source Entity", "Navigation",
		dunetask.Desc("Entity whose EstimatedState is watched"))

	// Task state: only touched by handlers and hooks (the task's goroutine).
	var lastWarning time.Time
	tooDeep := false

	t.OnUpdateParameters = func(changed []string) {
		if len(changed) > 0 {
			t.Inf("parameters changed: %s", strings.Join(changed, ", "))
		}
	}
	t.OnActivation = func() error {
		t.Inf("watching depth (maximum %.1f m)", maxDepth.Get())
		return nil
	}
	t.OnDeactivation = func() error {
		tooDeep = false
		t.State(imc.EntityStateState_NORMAL, "idle")
		return nil
	}
	dunetask.Consume(t, func(m *imc.EstimatedState) {
		if src, ok := t.EidOf(source.Get()); !t.Active() || !ok || m.Header.SrcEnt != src {
			return
		}
		depth := float64(m.Depth)
		if depth > maxDepth.Get() {
			t.State(imc.EntityStateState_FAULT, fmt.Sprintf("too deep: %.1f m", depth))
			now := time.Now()
			if lastWarning.IsZero() || now.Sub(lastWarning).Seconds() >= interval.Get() {
				lastWarning = now
				t.War("depth %.1f m is over %.1f m", depth, maxDepth.Get())
				t.Dispatch(&imc.TextMessage{Origin: t.Label(), Text: fmt.Sprintf("too deep: %.1f m", depth)})
			}
			tooDeep = true
		} else {
			if tooDeep {
				t.Inf("depth back to %.1f m", depth)
			}
			tooDeep = false
			t.State(imc.EntityStateState_NORMAL, "watching")
		}
	})

	dunetask.Main(dunetask.Options{Bus: *bus}, t)
}
