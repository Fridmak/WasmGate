// Package session owns the long-lived control-plane stream, transport setup,
// heartbeat and message dispatch. It depends on small options, identity and
// release interfaces rather than the root Agent.
package session
