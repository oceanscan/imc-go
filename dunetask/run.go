package dunetask

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// ErrStart wraps the error of a task's OnStart (Main exits with 1).
var ErrStart = errors.New("task failed to start")

// drainTimeout bounds the time spent flushing the outgoing queue when
// stopping (SIGTERM must end the program within 2 s).
const drainTimeout = time.Second

// Run runs tasks on one bus connection until ctx is done or the bus gives
// up. It returns nil when ctx ends, ErrIMCMismatch on an IMC mismatch with
// MismatchExit, or an error wrapping ErrStart when a task fails to start.
//
// On return the tasks are deactivated, OnStop was called, their entities
// reported BOOT "stopping" and the connection is closed.
func Run(ctx context.Context, opts Options, tasks ...*Task) error {
	b, err := NewBus(opts)
	if err != nil {
		return err
	}
	return b.Run(ctx, tasks...)
}

// Run runs tasks on this bus (see the function Run). A bus runs once.
func (b *Bus) Run(ctx context.Context, tasks ...*Task) error {
	if len(tasks) == 0 {
		return errors.New("dunetask: no tasks")
	}
	for _, t := range tasks {
		if err := t.attach(b); err != nil {
			return err
		}
	}
	var started []*Task
	stopAll := func() {
		for _, t := range started {
			t.stop()
		}
	}
	for _, t := range tasks {
		if err := t.start(ctx); err != nil {
			b.log.Errorf("%v", err)
			stopAll()
			return fmt.Errorf("%w: %v", ErrStart, err)
		}
		started = append(started, t)
	}
	// The connection outlives ctx until the tasks have said goodbye.
	bctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- b.run(bctx) }()

	var err error
	select {
	case <-ctx.Done():
	case err = <-runErr:
		runErr = nil
	}
	b.log.Infof("stopping")
	stopAll()
	b.drain(drainTimeout)
	cancel()
	if runErr != nil {
		err = <-runErr
	}
	if err != nil {
		b.log.Errorf("giving up: %v", err)
	}
	return err
}

// Main runs tasks until SIGTERM or SIGINT and exits the program: 0 when
// stopped by a signal, 1 when a task fails to start (or the options are
// wrong), 2 on an IMC mismatch. The options default to the environment
// that DUNE's Supervisors.Process sets (DUNE_BUS, DUNE_TASK_NAME,
// DUNE_SYSTEM; DUNE_LOG for the log level).
func Main(opts Options, tasks ...*Task) {
	os.Exit(MainCode(opts, tasks...))
}

// MainCode is Main without the exit: it returns the exit status.
func MainCode(opts Options, tasks ...*Task) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	err := Run(ctx, opts, tasks...)
	switch {
	case err == nil:
		return 0
	case errors.Is(err, ErrIMCMismatch):
		return 2
	default:
		out := opts.Output
		if out == nil {
			out = defaultOutput
		}
		out.Logger("dunetask").Errorf("fatal error: %v", err)
		return 1
	}
}
