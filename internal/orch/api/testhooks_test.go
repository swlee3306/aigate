package api

// pauseWorkers stops the daemon's worker loop once any Tick in flight has
// finished (the write lock waits for the readers), so a test can create tasks
// and move them through states by hand without the loop picking them up first.
// Without it, a task parked in state "queued" for a moment can be claimed and
// failed by the placeholder steps when the machine is under load.
func (d *Daemon) pauseWorkers() {
	d.tickMu.Lock()
	d.paused = true
	d.tickMu.Unlock()
}
