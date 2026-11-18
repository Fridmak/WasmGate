// Package server owns listener sockets and their lifecycle. It accepts client
// connections, dispatches them to conn.Handler, reconciles reloads, runs health
// probes, and drains active work during shutdown.
package server
