package dnsecho

import (
	"context"
	"testing"

	"github.com/jcodybaker/doks-net-monitor/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEcho(t *testing.T) {
	ctx := context.Background()
	s := NewServer(":0")
	require.NoError(t, s.Start(), "starting server")
	defer s.Stop(ctx)
	metrics := NewDNSClientMetrics()
	c := NewDNSTarget(0, s.s.PacketConn.LocalAddr().String(), metrics, types.TargetMetadata{
		RemoteNode: s.s.Addr,
		RemotePod:  s.s.Addr,
		TargetType: "test",
		LocalNode:  "foo",
		LocalPod:   "bar",
	})
	reset := func() {
		c.onError = nil
		c.onSuccess = nil
		c.onTxLoss = nil
		c.onDupe = nil
	}
	t.Run("success", func(t *testing.T) {
		defer reset()

		c.onError = func(err error) {
			t.Errorf("unexpected error: %v", err)
		}
		var successCount, txLossCount int
		c.onSuccess = func() {
			t.Log("success")
			successCount++
		}
		c.onTxLoss = func(diff uint16) {
			txLossCount++
		}
		c.onDupe = func() {
			t.Error("unexpected dupe")
		}
		c.probe(ctx)
		assert.Equal(t, 1, successCount)
		assert.Equal(t, 0, txLossCount)
	})

	t.Run("tx loss", func(t *testing.T) {
		c.onError = func(err error) {
			t.Errorf("unexpected error: %v", err)
		}
		var successCount, txLossCount int
		c.onSuccess = func() {
			t.Log("success")
			successCount++
		}
		c.onTxLoss = func(diff uint16) {
			txLossCount++
		}
		c.onDupe = func() {
			t.Error("unexpected dupe")
		}
		c.probeID++ // simulate a lost packet by incrementing the client-side probe ID
		c.probe(ctx)
		assert.Equal(t, 1, successCount)
		assert.Equal(t, 1, txLossCount)
	})

	t.Run("rollover", func(t *testing.T) {
		c.onError = func(err error) {
			t.Errorf("unexpected error: %v", err)
		}
		var successCount, txLossCount int
		c.onSuccess = func() {
			t.Log("success")
			successCount++
		}
		c.onTxLoss = func(diff uint16) {
			txLossCount++
		}
		c.onDupe = func() {
			t.Error("unexpected dupe")
		}
		c.probeID = 65534
		s.lastID[c.probeClientID] = 65534
		c.probe(ctx)
		c.probe(ctx)
		assert.Equal(t, 2, successCount)
		assert.Equal(t, 0, txLossCount)
	})
}
