# supervisor

A small in-process manager for long-lived background workers. Extracted from ctech-account, whose workers
(`deletion.RunWorker`, `planlimit.RunWorker`, `keystore.RunRotator`, `geoupdater.Run`) are started with
`go ...(context.Background(), ...)`: nothing cancels them at shutdown, a panic kills the process, and each
hand-rolls its own lock-per-tick.

## When to use

Any loop that must run in the background of an API process: sweepers, rotators, refreshers. Not a job queue.

## API

```go
sup := supervisor.New(ctx, supervisor.Config{
    Locker: lock.New(cacheBackend, 30*time.Second), // optional: one leader per worker across instances
    LockPrefix: "supervisor:" + env + ":",         // namespace per environment on a shared Valkey
    Drain: &drainTracker,                           // optional: stop workers when the process drains
})
sup.Run(ctx, "deletion", supervisor.Every(time.Minute, func(ctx context.Context) error {
    svc.ProcessDue(ctx)
    return nil
}))
// shutdown
shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
defer cancel()
_ = sup.Shutdown(shutdownCtx)
```

- `New(parent, Config)`, `Run(ctx, name, fn) error` (non-blocking), `Shutdown(ctx) error`.
- `Every(interval, tick)` adapts a tick function to a worker (tick errors are logged, loop continues).
- `ErrStopped` is returned by `Run` after shutdown/drain began.

## Semantics and decisions

- `fn` receives a cancellable context and must return when it is done. Returning `nil` means the work is
  finished (not restarted); returning an error or panicking restarts it after exponential backoff
  (1s doubling to 1m; reset when a run lasted at least the max backoff). No jitter (shortcut).
- **Leader election is fail-closed.** With a `Locker`, a worker runs only while its instance holds
  `<LockPrefix><name>`, renewed by heartbeat. If the lock store is unreachable, or a renewal fails, `fn`'s context
  is cancelled and nobody leads until the store answers: a singleton worker pauses rather than running on every
  instance. Without a `Locker` every instance runs every worker (use for per-instance work such as key reloads).
  The in-memory locker is not shared across replicas.
- The Locker's TTL must comfortably exceed `HeartbeatInterval` (default 5s).
- Shutdown is cooperative: the contexts are cancelled and `Shutdown` waits, bounded by its `ctx`. On timeout it
  returns the error and stragglers keep running, so exit the process.
- With `Config.Drain`, the supervisor registers in the `drain.Tracker`; `Tracker.Drain` stops all workers and
  `Run` refuses new ones.
- Failures are logged via `observability.Error`; no metrics.

## Migration (ctech-account)

| Today | After |
|---|---|
| `go deletion.RunWorker(ctx, svc, tryLock, interval)` and its tick-scoped `tryLock` | `sup.Run(ctx, "deletion", supervisor.Every(interval, ...))` with `Config.Locker`; drop the `tryLock` closure |
| `go planlimit.RunWorker(...)` | same |
| `go keystore.RunRotator(...)` | keep its own rotation lock (it is a per-rotation lock released early); run it with `sup.Run` and no `Locker` since every instance must reload keys |
| `go geoupdater.Run(...)` | `sup.Run(ctx, "geoupdater", ...)` |
| `cmd/api/main.go` `context.Background()` | `sup.Shutdown(ctx)` in the shutdown path, and `Drain` wired to the existing tracker |
