// Package release verifies signed control-plane releases, restores artifacts,
// validates configuration and applies it through a small reloader interface.
// It does not depend on the root Agent or session lifecycle.
package release
