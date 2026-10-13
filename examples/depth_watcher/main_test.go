package main

// The example as DUNE's Supervisors.Process starts it: no arguments, the
// bus and names in the environment; over TCP and over a Unix socket.

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	imc "github.com/oceanscan/imc-go"
	"github.com/oceanscan/imc-go/dunetask/dunetest"
)

// syncBuffer is a bytes.Buffer safe for the process's output goroutine.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestRunsWithoutArguments(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "depth_watcher")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	for _, transport := range []string{"tcp", "unix"} {
		t.Run(transport, func(t *testing.T) {
			cfg := dunetest.Config{}
			if transport == "unix" {
				// Short path: sun_path holds 107 bytes.
				d, err := os.MkdirTemp("", "dw")
				if err != nil {
					t.Fatal(err)
				}
				defer os.RemoveAll(d)
				cfg.Path = filepath.Join(d, "bus.sock")
			}
			srv, err := dunetest.Start(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer srv.Close()
			cmd := exec.Command(bin)
			cmd.Env = append(os.Environ(), "DUNE_BUS="+srv.URL(), "DUNE_TASK_NAME=Supervisors.Process/Depth Watcher",
				"DUNE_SYSTEM=fake")
			var out syncBuffer
			cmd.Stdout, cmd.Stderr = &out, &out
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer cmd.Process.Kill()
			eid, err := srv.WaitEntity("Depth Watcher", 20*time.Second)
			if err != nil {
				t.Fatalf("%v\n%s", err, out.String())
			}
			c := srv.Clients()[0]
			if c.Keys()["#name"] != "Supervisors.Process/Depth Watcher" || !c.Sub()[imc.IDEstimatedState] {
				t.Fatalf("keys %v", c.Keys())
			}
			wait := func(match func(imc.IMessage) bool) imc.IMessage {
				t.Helper()
				m, err := srv.WaitMessage(match, 10*time.Second)
				if err != nil {
					t.Fatalf("%v\n%s", err, out.String())
				}
				return m
			}
			wait(dunetest.Where(func(m *imc.EntityState) bool { return int(m.Header.SrcEnt) == eid }))
			srv.Send(&imc.SetEntityParameters{Name: "Depth Watcher", Params: []*imc.EntityParameter{
				{Name: "Active", Value: "true"}, {Name: "Maximum Depth", Value: "1"}}}, "", "")
			wait(dunetest.Where(func(m *imc.EntityState) bool { return int(m.Header.SrcEnt) == eid && m.Description == "active" }))
			srv.Send(&imc.EstimatedState{Depth: 3}, "Navigation", "")
			m := wait(dunetest.Of[*imc.TextMessage]()).(*imc.TextMessage)
			if m.Text != "too deep: 3.0 m" {
				t.Fatal(m.Text)
			}
			wait(dunetest.Where(func(m *imc.EntityState) bool {
				return m.State == imc.EntityStateState_FAULT && m.Description == "too deep: 3.0 m"
			}))
			wait(dunetest.Where(func(m *imc.LogBookEntry) bool {
				return m.Type == imc.LogBookEntryType_WARNING && m.Text == "depth 3.0 m is over 1.0 m"
			}))
			cmd.Process.Signal(syscall.SIGTERM)
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("exit: %v\n%s", err, out.String())
				}
			case <-time.After(2 * time.Second):
				t.Fatal("not stopped within 2 s")
			}
			// No time stamps under DUNE (it adds its own).
			first := strings.SplitN(out.String(), "\n", 2)[0]
			if !strings.HasPrefix(first, "INFO dunetask: connected to "+srv.URL()) {
				t.Fatalf("first line %q", first)
			}
		})
	}
}
