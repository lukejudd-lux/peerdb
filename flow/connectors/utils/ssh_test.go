package utils

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"github.com/PeerDB-io/peerdb/flow/generated/protos"
)

func TestCachedSigner_ParsesOnceAndReuses(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	block, err := ssh.MarshalPrivateKey(priv, "")
	require.NoError(t, err)
	b64 := base64.StdEncoding.EncodeToString(pem.EncodeToMemory(block))

	s1, err := cachedSigner(b64)
	require.NoError(t, err)
	s2, err := cachedSigner(b64)
	require.NoError(t, err)
	require.Same(t, s1, s2, "the same private key must return the cached signer instance")

	cfg, err := GetSSHClientConfig(&protos.SSHConfig{User: "testuser", PrivateKey: b64})
	require.NoError(t, err)
	require.Len(t, cfg.Auth, 1, "private key config should yield one auth method")
}

func startTestKeepaliveLoop(
	t *testing.T, interval time.Duration, maxStrikes int, send func() error, onFailure func(),
) *SSHTunnel {
	t.Helper()
	tunnel := &SSHTunnel{logger: slog.New(slog.DiscardHandler)}
	stopChan := make(chan struct{})
	tunnel.keepaliveChan.Store(&stopChan)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	go tunnel.runKeepaliveLoopWith(ctx, stopChan, onFailure, keepaliveOptions{
		interval:   interval,
		maxStrikes: maxStrikes,
		send:       send,
	})
	return tunnel
}

func TestKeepaliveSingleDelayedReplyDoesNotFailTunnel(t *testing.T) {
	t.Parallel()
	interval := 25 * time.Millisecond
	var calls atomic.Int32
	send := func() error {
		if calls.Add(1) == 1 {
			time.Sleep(3 * interval / 2)
		}
		return nil
	}

	failed := make(chan struct{})
	tunnel := startTestKeepaliveLoop(t, interval, SSHKeepaliveMaxStrikes, send, func() {
		close(failed)
	})

	select {
	case <-failed:
		t.Fatal("a single delayed keepalive reply should not mark the tunnel bad")
	case <-time.After(8 * interval):
	}
	require.False(t, tunnel.IsBad())
	require.GreaterOrEqual(t, calls.Load(), int32(2))
}

func TestKeepaliveHungRequestFailsAfterMaxStrikes(t *testing.T) {
	t.Parallel()
	interval := 20 * time.Millisecond
	failed := make(chan struct{})
	tunnel := startTestKeepaliveLoop(t, interval, SSHKeepaliveMaxStrikes, func() error {
		<-t.Context().Done()
		return t.Context().Err()
	}, func() { close(failed) })

	select {
	case <-failed:
	case <-time.After(time.Duration(SSHKeepaliveMaxStrikes+3) * interval):
		t.Fatal("hung keepalive should mark the tunnel bad after consecutive strikes")
	}
	require.True(t, tunnel.IsBad())
}

func TestKeepaliveHardErrorFailsImmediately(t *testing.T) {
	t.Parallel()
	interval := 20 * time.Millisecond
	failed := make(chan struct{})
	tunnel := startTestKeepaliveLoop(t, interval, SSHKeepaliveMaxStrikes, func() error {
		return errors.New("connection reset")
	}, func() { close(failed) })

	select {
	case <-failed:
	case <-time.After(3 * interval):
		t.Fatal("a keepalive send error should mark the tunnel bad without waiting for strikes")
	}
	require.True(t, tunnel.IsBad())
}

func TestSSHTunnel_GetKeepaliveChan_NilCases(t *testing.T) {
	// Nil tunnel
	var tunnel *SSHTunnel = nil
	require.Nil(t, tunnel.GetKeepaliveChan(context.Background()), "Nil tunnel should return nil channel")

	// Nil client
	tunnel = &SSHTunnel{}
	require.Nil(t, tunnel.GetKeepaliveChan(context.Background()), "Tunnel with nil client should return nil channel")

	// Bad tunnel
	tunnel = &SSHTunnel{}
	tunnel.badTunnel.Store(true)
	require.Nil(t, tunnel.GetKeepaliveChan(context.Background()), "Bad tunnel should return nil channel")
}

func TestSSHTunnel_StartKeepalive_NilCases(t *testing.T) {
	called := false
	onFailure := func() { called = true }

	var tunnel *SSHTunnel = nil
	tunnel.StartKeepalive(context.Background(), onFailure)
	require.False(t, called, "Nil tunnel should not call onFailure")

	tunnel = &SSHTunnel{}
	tunnel.StartKeepalive(context.Background(), onFailure)
	require.False(t, called, "Tunnel with nil client should not call onFailure")

	tunnel = &SSHTunnel{}
	tunnel.badTunnel.Store(true)
	tunnel.StartKeepalive(context.Background(), onFailure)
	require.False(t, called, "Bad tunnel should not call onFailure")
}

func TestSSHTunnel_Close_NilCases(t *testing.T) {
	// Nil tunnel
	var tunnel *SSHTunnel = nil
	require.NoError(t, tunnel.Close(), "Closing nil tunnel should not error")

	// Nil client
	tunnel = &SSHTunnel{}
	err := tunnel.Close()
	require.NoError(t, err, "Closing tunnel with nil client should not error")

	// Double close
	err2 := tunnel.Close()
	require.NoError(t, err2, "Second close should not error")
}
