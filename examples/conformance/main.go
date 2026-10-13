// Command conformance is the SDK conformance task (CT) of DUNE's
// doc/specs/SDK.Conformance.md in Go: the same behaviour as the Python
// (examples/python/conformance_task.py) and Rust CTs, so that one
// language-neutral suite (tests/sdk/conformance) can drive them all.
//
//	conformance [--bus URL] [--label LABEL] [--system SYSTEM]
//
// One entity (default label "Conformance Task") that honours Active, with
// parameters Gain (0 to 100, default 1.0) and Prefix (default "ct"). While
// active, every EstimatedState is answered with DevDataText
// "<Prefix>:<n>:<depth*Gain, 3 decimals>". DevDataText commands: ct-war,
// ct-err (log a warning / an error), ct-crash (exit 3 at once), ct-state
// (DevDataText "ct-state:<active>:<Gain>:<Prefix>").
package main

import (
	"flag"
	"fmt"
	"os"

	imc "github.com/oceanscan/imc-go"
	"github.com/oceanscan/imc-go/dunetask"
)

func main() {
	bus := flag.String("bus", "", "tcp://host:port or unix:///path (default: $DUNE_BUS or "+dunetask.DefaultBus+")")
	label := flag.String("label", "Conformance Task", "entity label")
	system := flag.String("system", "", "system id (decimal or 0x hex) or name (default: $DUNE_SYSTEM)")
	flag.Parse()

	t := dunetask.New(*label, dunetask.HonoursActive())
	gain := dunetask.NewParam(t, "Gain", 1.0, dunetask.Min(0), dunetask.Max(100), dunetask.Scope("maneuver"),
		dunetask.Desc("Factor applied to the depth"))
	prefix := dunetask.NewParam(t, "Prefix", "ct", dunetask.Scope("maneuver"),
		dunetask.Desc("Prefix of the DevDataText outputs"))

	// The counter is only touched by handlers and hooks, which run one at
	// a time on the task's goroutine.
	count := 0
	t.OnDeactivation = func() error {
		count = 0
		return nil
	}
	dunetask.Consume(t, func(m *imc.EstimatedState) {
		if !t.Active() {
			return
		}
		count++
		t.Dispatch(&imc.DevDataText{Value: fmt.Sprintf("%s:%d:%.3f", prefix.Get(), count, float64(m.Depth)*gain.Get())})
	})
	dunetask.Consume(t, func(m *imc.DevDataText) {
		switch m.Value {
		case "ct-war":
			t.War("conformance warning")
		case "ct-err":
			t.Err("conformance error")
		case "ct-crash":
			os.Exit(3)
		case "ct-state":
			active := 0
			if t.Active() {
				active = 1
			}
			t.Dispatch(&imc.DevDataText{Value: fmt.Sprintf("ct-state:%d:%s:%s", active, t.ParamText("Gain"), t.ParamText("Prefix"))})
		}
	})

	// The SDK's default gives up on an IMC mismatch; the CT keeps retrying
	// every 5 s (the minimum).
	dunetask.Main(dunetask.Options{Bus: *bus, System: *system, OnMismatch: dunetask.MismatchRetry,
		MismatchRetry: dunetask.MinMismatchRetry}, t)
}
