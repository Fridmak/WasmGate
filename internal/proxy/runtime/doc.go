// Package runtime owns immutable proxy snapshots and the long-lived state used
// to build them. It publishes configuration atomically and retains plugin,
// limiter and upstream state safely across reloads.
package runtime
