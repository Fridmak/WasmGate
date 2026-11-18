// Package identity manages the agent id, private key, certificate and CA files.
// It also performs enrollment and certificate renewal through an injected
// control-plane dialer, keeping transport concerns outside the identity code.
package identity
