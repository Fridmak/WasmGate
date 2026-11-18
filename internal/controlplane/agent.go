package controlplane

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"wasmgate/internal/controlplane/identity"
	"wasmgate/internal/controlplane/options"
	"wasmgate/internal/controlplane/release"
	"wasmgate/internal/controlplane/session"
)

// Options is the compatibility alias for the shared control-plane options.
type Options = options.Options

// Reloader applies a new configuration to the running data plane.
type Reloader = release.Reloader

const (
	DefaultHeartbeat        = options.DefaultHeartbeat
	DefaultReconnectBackoff = options.DefaultReconnectBackoff
	DefaultReconnectCeiling = options.DefaultReconnectCeiling
	DefaultRenewBefore      = options.DefaultRenewBefore
)

// Agent keeps one data plane connected to the control plane.
type Agent struct {
	opts       options.Options
	log        *slog.Logger
	reloader   release.Reloader
	releaseKey *ecdsa.PublicKey
}

// NewAgent validates opts and builds a control-plane client.
func NewAgent(opts Options, reloader Reloader, log *slog.Logger) (*Agent, error) {
	if reloader == nil {
		return nil, errors.New("control plane: reloader is required")
	}
	if err := opts.Normalize(); err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	releaseKey, err := release.LoadReleaseKey(opts.ReleaseKey)
	if err != nil {
		return nil, err
	}
	return &Agent{opts: opts, log: log, reloader: reloader, releaseKey: releaseKey}, nil
}

// Run keeps the session alive until ctx is cancelled. Connection failures are
// retried with an exponential backoff; a cancelled context returns nil.
func (a *Agent) Run(ctx context.Context) error {
	state := session.NewStateStore(a.opts.StateDir, session.LoadApplied(a.opts.StateDir))
	backoff := a.opts.ReconnectMin
	for {
		if ctx.Err() != nil {
			return nil
		}
		started := time.Now()
		err := a.runOnce(ctx, state)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			a.log.Warn("control-plane session ended", "err", err)
		} else {
			a.log.Info("control-plane session closed")
		}
		if time.Since(started) >= a.opts.ReconnectMax {
			backoff = a.opts.ReconnectMin
		}
		a.log.Info("reconnecting to control plane", "endpoint", a.opts.Endpoint, "in", backoff)
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
		backoff *= 2
		if backoff > a.opts.ReconnectMax {
			backoff = a.opts.ReconnectMax
		}
	}
}

func (a *Agent) runOnce(ctx context.Context, state *session.StateStore) error {
	id, err := identity.Load(a.opts.StateDir)
	switch {
	case errors.Is(err, identity.ErrIncompleteIdentity):
		a.log.Warn("incomplete agent identity, enrolling again", "state_dir", a.opts.StateDir)
		id, err = a.enroll(ctx)
		if err != nil {
			return fmt.Errorf("enrollment: %w", err)
		}
	case err != nil:
		return err
	case id == nil:
		id, err = a.enroll(ctx)
		if err != nil {
			return fmt.Errorf("enrollment: %w", err)
		}
	}

	if id != nil && !strings.HasPrefix(strings.ToLower(a.opts.Endpoint), "http://") {
		remaining := time.Until(id.NotAfter)
		switch {
		case remaining <= 0:
			a.log.Error("agent certificate expired", "expired_at", id.NotAfter)
			if strings.TrimSpace(a.opts.EnrollToken) == "" {
				return fmt.Errorf("control plane: certificate expired at %s; issue an enrollment token and restart", id.NotAfter.Format(time.RFC3339))
			}
			id, err = a.enroll(ctx)
			if err != nil {
				return fmt.Errorf("re-enrollment: %w", err)
			}
		case remaining < a.opts.RenewBefore:
			renewed, renewErr := a.renew(ctx, id)
			if renewErr != nil {
				a.log.Warn("certificate renewal failed; continuing with the current certificate", "err", renewErr, "expires_at", id.NotAfter)
			} else {
				id = renewed
			}
		}
	}

	conn, err := (session.Dialer{Options: a.opts}).Dial(ctx, id)
	if err != nil {
		return err
	}
	defer conn.Close() //nolint:errcheck
	return session.Run(ctx, conn, id, a.opts, a.releaseKey, a.reloader, state, a.log)
}

func (a *Agent) enroll(ctx context.Context) (*identity.Identity, error) {
	return identity.Enroll(ctx, a.opts, session.Dialer{Options: a.opts}, a.log)
}

func (a *Agent) renew(ctx context.Context, current *identity.Identity) (*identity.Identity, error) {
	return identity.Renew(ctx, a.opts, current, session.Dialer{Options: a.opts}, a.log)
}
