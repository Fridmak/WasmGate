// Package conn implements the per-connection proxy pipeline. It pins a runtime
// snapshot, applies limits and plugin hooks, dials a backend, and delegates
// byte forwarding to relay or HTTP inspection.
package conn
