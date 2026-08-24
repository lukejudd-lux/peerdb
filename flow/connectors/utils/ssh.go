package utils

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/PeerDB-io/peerdb/flow/generated/protos"
	"github.com/PeerDB-io/peerdb/flow/internal"
	"github.com/PeerDB-io/peerdb/flow/shared"
	"github.com/PeerDB-io/peerdb/flow/shared/exceptions"
)

const (
	SSHKeepaliveInterval = 15 * time.Second
	// OpenSSH ClientAliveCountMax default. The jump host (Ubuntu sshd) does not set
	// ClientAliveInterval, so a single delayed reply is lag, not a dead tunnel.
	SSHKeepaliveMaxStrikes  = 3
	SSHKeepaliveHungTimeout = (SSHKeepaliveMaxStrikes + 2) * SSHKeepaliveInterval

	// sshd LoginGraceTime is 120s. Bound handshake so a hung kex cannot occupy a
	// MaxStartups slot (bastion is 50:30:200, shared with Fivetran) until then.
	SSHDialTimeout = 30 * time.Second
	// sshd TCPKeepAlive is yes; probe faster than the kernel 2h default.
	SSHTCPKeepaliveInterval = 30 * time.Second
)

type keepaliveOptions struct {
	interval   time.Duration
	maxStrikes int
	send       func() error
}

type SSHTunnel struct {
	*ssh.Client
	logger        *slog.Logger
	keepaliveChan atomic.Pointer[chan struct{}]
	badTunnel     atomic.Bool
}

// GetSSHClientConfig returns an *ssh.ClientConfig based on provided credentials.
func GetSSHClientConfig(config *protos.SSHConfig) (*ssh.ClientConfig, error) {
	var authMethods []ssh.AuthMethod

	// Password-based authentication
	if config.Password != "" {
		authMethods = append(authMethods, ssh.Password(config.Password))
	}

	// Private key-based authentication
	if config.PrivateKey != "" {
		pkey, err := base64.StdEncoding.DecodeString(config.PrivateKey)
		if err != nil {
			return nil, fmt.Errorf("failed to base64 decode private key: %w", err)
		}

		signer, err := ssh.ParsePrivateKey(pkey)
		if err != nil {
			return nil, fmt.Errorf("failed to parse private key: %w", err)
		}

		authMethods = append(authMethods, ssh.PublicKeys(signer))
	}

	if len(authMethods) == 0 {
		return nil, fmt.Errorf("no authentication methods provided")
	}

	var hostKeyCallback ssh.HostKeyCallback
	if config.HostKey != "" {
		pubKey, _, _, _, err := ssh.ParseAuthorizedKey([]byte(config.HostKey))
		if err != nil {
			return nil, fmt.Errorf("failed to parse host key: %w", err)
		}
		hostKeyCallback = ssh.FixedHostKey(pubKey)
	} else {
		//nolint:gosec
		hostKeyCallback = ssh.InsecureIgnoreHostKey()
	}

	return &ssh.ClientConfig{
		User:            config.User,
		Auth:            authMethods,
		HostKeyCallback: hostKeyCallback,
		Timeout:         SSHDialTimeout,
	}, nil
}

func NewSSHTunnel(
	ctx context.Context,
	sshConfig *protos.SSHConfig,
) (*SSHTunnel, error) {
	if sshConfig != nil {
		logger := internal.LoggerFromCtx(ctx)
		sshServer := shared.JoinHostPort(sshConfig.Host, sshConfig.Port)
		clientConfig, err := GetSSHClientConfig(sshConfig)
		if err != nil {
			logger.Error("Failed to get SSH client config", slog.Any("error", err))
			return nil, err
		}

		logger.Info("Setting up SSH connection", slog.String("Server", sshServer))
		client, err := dialSSH(ctx, sshServer, clientConfig)
		if err != nil {
			return nil, exceptions.NewSSHTunnelSetupError(err)
		}

		return &SSHTunnel{
			Client: client,
			logger: internal.SlogLoggerFromCtx(ctx),
		}, nil
	}

	return nil, nil
}

func dialSSH(ctx context.Context, addr string, clientConfig *ssh.ClientConfig) (*ssh.Client, error) {
	dialer := &net.Dialer{
		Timeout:   SSHDialTimeout,
		KeepAlive: SSHTCPKeepaliveInterval,
	}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			_ = conn.Close()
			return nil, ctx.Err()
		}
		cfg := *clientConfig
		if cfg.Timeout == 0 || remaining < cfg.Timeout {
			cfg.Timeout = remaining
		}
		clientConfig = &cfg
	}
	clientConn, chans, reqs, err := ssh.NewClientConn(conn, addr, clientConfig)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return ssh.NewClient(clientConn, chans, reqs), nil
}

// IsBad reports whether the tunnel has been marked unusable (keepalive failure or explicit close).
// Safe to call on a nil tunnel, in which case it returns false.
func (tunnel *SSHTunnel) IsBad() bool {
	return tunnel != nil && tunnel.badTunnel.Load()
}

func (tunnel *SSHTunnel) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	conn, err := tunnel.Client.DialContext(ctx, network, address)
	if err != nil {
		return nil, exceptions.NewSSHTunnelDialError(err)
	}
	return NewDeadlineCapableConn(conn), nil
}

func (tunnel *SSHTunnel) Close() error {
	if tunnel != nil && tunnel.Client != nil {
		tunnel.markTunnelBad(nil)
		return tunnel.Client.Close()
	}
	return nil
}

func (tunnel *SSHTunnel) closeKeepaliveChan() {
	if keepaliveChan := tunnel.keepaliveChan.Swap(nil); keepaliveChan != nil {
		close(*keepaliveChan)
	}
}

func (tunnel *SSHTunnel) markTunnelBad(onFailure func()) {
	tunnel.closeKeepaliveChan()
	tunnel.badTunnel.Store(true)
	if onFailure != nil {
		onFailure()
	}
}

func (tunnel *SSHTunnel) runKeepaliveLoop(
	ctx context.Context, stopChan <-chan struct{}, onFailure func(),
) {
	tunnel.runKeepaliveLoopWith(ctx, stopChan, onFailure, keepaliveOptions{
		interval:   SSHKeepaliveInterval,
		maxStrikes: SSHKeepaliveMaxStrikes,
		send: func() error {
			_, _, err := tunnel.Client.SendRequest("keepalive@openssh.com", true, nil)
			return err
		},
	})
}

func (tunnel *SSHTunnel) runKeepaliveLoopWith(
	ctx context.Context, stopChan <-chan struct{}, onFailure func(), opts keepaliveOptions,
) {
	ticker := time.NewTicker(opts.interval)
	defer ticker.Stop()
	logger := tunnel.logger
	requestSent := atomic.Bool{}
	var strikes atomic.Int32
	var keepaliveErr error
	errChan := make(chan struct{})
	var errOnce sync.Once

	for {
		select {
		case <-ticker.C:
			if requestSent.Load() {
				n := strikes.Add(1)
				if int(n) < opts.maxStrikes {
					logger.WarnContext(ctx, "Keepalive request still pending",
						slog.Int("strikes", int(n)),
						slog.Int("maxStrikes", opts.maxStrikes),
					)
					continue
				}
				logger.ErrorContext(ctx, "Keepalive request still pending, marking tunnel as bad",
					slog.Int("strikes", int(n)),
					slog.Int("maxStrikes", opts.maxStrikes),
				)
				tunnel.markTunnelBad(onFailure)
				return
			}
			requestSent.Store(true)
			go func() {
				err := opts.send()
				if err != nil {
					keepaliveErr = err
					errOnce.Do(func() { close(errChan) })
					return
				}
				requestSent.Store(false)
				strikes.Store(0)
			}()
		case <-ctx.Done():
			tunnel.closeKeepaliveChan()
			return
		case <-stopChan:
			return
		case <-errChan:
			logger.ErrorContext(ctx, "Keepalive request failed, marking tunnel as bad", slog.Any("error", keepaliveErr))
			tunnel.markTunnelBad(onFailure)
			return
		}
	}
}

func (tunnel *SSHTunnel) StartKeepalive(ctx context.Context, onFailure func()) {
	if tunnel == nil || tunnel.Client == nil || tunnel.badTunnel.Load() {
		return
	}
	if tunnel.keepaliveChan.Load() != nil {
		// Already started
		return
	}
	stopChan := make(chan struct{})
	tunnel.keepaliveChan.Store(&stopChan)

	go tunnel.runKeepaliveLoop(ctx, stopChan, onFailure)
}

// returns a channel that is closed if the SSH keepalive fails,
// or nil if no SSH tunnel is configured
func (tunnel *SSHTunnel) GetKeepaliveChan(ctx context.Context) <-chan struct{} {
	if tunnel == nil || tunnel.Client == nil || tunnel.badTunnel.Load() {
		// nil channel would be of no consequence in a select
		// UNLESS it's the only branch in a select, in which case it would block forever
		return nil
	}
	if keepaliveChan := tunnel.keepaliveChan.Load(); keepaliveChan != nil {
		// Already started
		return *keepaliveChan
	}
	keepaliveChan := make(chan struct{})
	tunnel.keepaliveChan.Store(&keepaliveChan)

	go tunnel.runKeepaliveLoop(ctx, keepaliveChan, nil)
	return keepaliveChan
}
