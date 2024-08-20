package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog/log"
	v1 "k8s.io/api/core/v1"
)

var (
	dialer = &net.Dialer{
		Timeout:   5 * time.Second,
		KeepAlive: 5 * time.Second,
	}
)

// TCPMetrics holds metrics roots for all probers in this process.
type TCPMetrics struct {
	ConnectCount     *prometheus.CounterVec
	ProbeCount       *prometheus.CounterVec
	ProbeLatencyHist *prometheus.HistogramVec
}

// NewTCPMetrics creates a metrics root for TCPTarget probers.
func NewTCPMetrics() *TCPMetrics {
	return &TCPMetrics{
		ConnectCount: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "tcp_persistent_prober",
			Subsystem: "connect",
			Name:      "count",
			Help:      "Total count of connect attempts by outcome.",
		}, []string{"target", "target_node", "target_pod", "target_type", "local_node", "local_pod", "outcome", "error"}),
		ProbeCount: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "tcp_persistent_prober",
			Subsystem: "probe",
			Name:      "count",
			Help:      "Total count of probes by outcome.",
		}, []string{"target", "target_node", "target_pod", "target_type", "local_node", "local_pod", "outcome", "error"}),
		ProbeLatencyHist: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace: "tcp_persistent_prober",
				Subsystem: "probe",
				Name:      "latency",
				Help:      "Latency of probes.",
				Buckets:   prometheus.DefBuckets,
			}, []string{"target", "target_node", "target_pod", "target_type", "local_node", "local_pod"},
		),
	}
}

func (m *TCPMetrics) Register(reg prometheus.Registerer) error {
	if err := reg.Register(m.ConnectCount); err != nil {
		return err
	}
	if err := reg.Register(m.ProbeCount); err != nil {
		return err
	}
	if err := reg.Register(m.ProbeLatencyHist); err != nil {
		return err
	}
	return nil
}

// TCPTarget connects to a TCP destination and pings on a regular interval, reporting statistics.
type TCPTarget struct {
	Addr string
	uuid string
	*TCPMetrics
	stop      context.CancelFunc
	mutex     sync.Mutex
	localNode *v1.Node
	localPod  *v1.Pod
	metadata  TargetMetadata
}

// Payload is the wire message transmitted by TCPTarget
type Payload struct {
	UUID     string `json:"uuid"`
	NodeName string `json:"node_name"`
	PodName  string `json:"pod_name"`
	Payload  string `json:"payload"`
	Hangup   bool   `json:"hangup,omitempty"`
}

type TargetMetadata struct {
	RemoteNode string
	RemotePod  string
	TargetType string
	LocalNode  string
	LocalPod   string
}

// NewTCPTarget creates a new TCPTarget.
func NewTCPTarget(probeInterval time.Duration, addr string, m *TCPMetrics, metadata TargetMetadata) *TCPTarget {
	return &TCPTarget{
		Addr:       addr,
		uuid:       uuid.New().String(),
		TCPMetrics: m,
		metadata:   metadata,
	}
}

func (t *TCPTarget) Run(ctx context.Context) {
	ll := log.Ctx(ctx).With().
		Str("component", "probe").
		Str("target", t.Addr).
		Str("target_type", t.metadata.TargetType).
		Str("target_node", t.metadata.RemoteNode).
		Str("target_pod", t.metadata.RemotePod).
		Str("local_node", t.metadata.LocalNode).
		Str("local_pod", t.metadata.LocalPod).
		Logger()
	ctx = ll.WithContext(ctx)
	t.mutex.Lock()
	if t.stop != nil {
		ll.Warn().Msg("tcp target already running")
		t.mutex.Unlock()
	}
	ctx, stop := context.WithCancel(ctx)
	t.stop = stop
	t.mutex.Unlock()
	defer func() {
		ll.Info().Msg("stopping probes")
		stop()
		t.mutex.Lock()
		t.stop = nil
		t.mutex.Unlock()
	}()
	for ctx.Err() == nil {
		conn, err := dialer.DialContext(ctx, "tcp", t.Addr)
		if err != nil {
			if ctx.Err() != nil {
				// If the parent ctx is cancelled we should ignore whatever error dial gives us.
				ll.Info().Msg("cancelling probe dial: shutting down")
				return
			}
			t.ConnectCount.With(prometheus.Labels{
				"target":      t.Addr,
				"target_node": t.metadata.RemoteNode,
				"target_pod":  t.metadata.RemotePod,
				"target_type": t.metadata.TargetType,
				"local_node":  t.metadata.LocalNode,
				"local_pod":   t.metadata.LocalPod,
				"outcome":     "error",
				"error":       err.Error(),
			}).Inc()
			ll.Err(err).Msg("connecting to remote")
			continue
		}
		t.ConnectCount.With(prometheus.Labels{
			"target":      t.Addr,
			"target_node": t.metadata.RemoteNode,
			"target_pod":  t.metadata.RemotePod,
			"target_type": t.metadata.TargetType,
			"local_node":  t.metadata.LocalNode,
			"local_pod":   t.metadata.LocalPod,
			"outcome":     "success",
			"error":       "",
		}).Inc()
		ll = ll.With().Str("remote_addr", conn.RemoteAddr().String()).Str("local_addr", conn.LocalAddr().String()).Logger()
		ll.Info().Msg("connected to remote")
		t.probe(ll.WithContext(ctx), conn)
	}
}

func (t *TCPTarget) Stop() {
	t.mutex.Lock()
	if t.stop != nil {
		t.stop()
	}
	t.mutex.Unlock()
}

func (t *TCPTarget) probe(ctx context.Context, conn net.Conn) {
	// copy metadata since we may amend it based on data from the remote.  That is only valid for
	// this connection so we don't want to modify the original metadata.
	metadata := t.metadata
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	defer conn.Close()
	j := json.NewEncoder(conn)
	payload := &Payload{UUID: t.uuid}
	if t.localNode != nil {
		payload.NodeName = t.localNode.Name
	}
	if t.localPod != nil {
		payload.PodName = t.localPod.Name
	}
	scanner := bufio.NewScanner(conn)
	for ctx.Err() == nil {
		var resp Payload
		start := time.Now()
		if err := j.Encode(payload); err != nil {
			// It's possible the remote indicated they need to hangup because they're being closed.
			// Try a read to see if there's a hangup message.
			if scanner.Scan() {
				if err := json.Unmarshal(scanner.Bytes(), &resp); err == nil && resp.Hangup {
					// Clean hangup from remote, no error.
					select {
					case <-ctx.Done():
					case <-time.After(5 * time.Second):
						// The remote is shutting down. It's likely its being terminated / replaced
						// but it might take a few moments for the Kubernetes discover to catch-up.
					}
					return
				}
				t.ProbeCount.With(prometheus.Labels{
					"target":      t.Addr,
					"target_node": metadata.RemoteNode,
					"target_pod":  metadata.RemotePod,
					"target_type": metadata.TargetType,
					"local_node":  metadata.LocalNode,
					"local_pod":   metadata.LocalPod,
					"outcome":     "write_error",
					"error":       err.Error(),
				}).Inc()
			}
			return
		}

		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				t.ProbeCount.With(prometheus.Labels{
					"target":      t.Addr,
					"target_node": metadata.RemoteNode,
					"target_pod":  metadata.RemotePod,
					"target_type": metadata.TargetType,
					"local_node":  metadata.LocalNode,
					"local_pod":   metadata.LocalPod,
					"outcome":     "read_error",
					"error":       err.Error(),
				}).Inc()
				log.Ctx(ctx).Err(err).Msg("read error")
				return
			}
			// Scanner.Err eats EOF assuming its just the end of the input, but we should never see an EOF w/o a hangup message
			t.ProbeCount.With(prometheus.Labels{
				"target":      t.Addr,
				"target_node": metadata.RemoteNode,
				"target_pod":  metadata.RemotePod,
				"target_type": metadata.TargetType,
				"local_node":  metadata.LocalNode,
				"local_pod":   metadata.LocalPod,
				"outcome":     "read_error",
				"error":       io.EOF.Error(),
			}).Inc()
			log.Ctx(ctx).Err(io.EOF).Msg("read error")
			return
		}

		if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
			t.ProbeCount.With(prometheus.Labels{
				"target":      t.Addr,
				"target_node": metadata.RemoteNode,
				"target_pod":  metadata.RemotePod,
				"target_type": metadata.TargetType,
				"local_node":  metadata.LocalNode,
				"local_pod":   metadata.LocalPod,
				"outcome":     "read_error",
				"error":       fmt.Sprintf("parse error: %v", err),
			}).Inc()
			log.Ctx(ctx).Err(err).Msg("parse error")
			return
		}

		if (metadata.RemoteNode == "" || metadata.RemotePod == "") && (resp.NodeName != "" || resp.PodName != "") {
			// The remote provided this info. It's only valid for this connection so we only write to the metadata
			// var in this scope (not on the receiver).
			metadata.RemoteNode = resp.NodeName
			metadata.RemotePod = resp.PodName
			ctx = log.Ctx(ctx).With().
				Str("target_node", metadata.RemoteNode).
				Str("target_pod", metadata.RemotePod).
				Logger().WithContext(ctx)
			log.Ctx(ctx).Info().Msg("remote identified")
		}

		latency := time.Since(start)
		t.ProbeCount.With(prometheus.Labels{
			"target":      t.Addr,
			"target_node": metadata.RemoteNode,
			"target_pod":  metadata.RemotePod,
			"target_type": metadata.TargetType,
			"local_node":  metadata.LocalNode,
			"local_pod":   metadata.LocalPod,
			"outcome":     "success",
			"error":       "",
		}).Inc()
		t.ProbeLatencyHist.With(prometheus.Labels{
			"target":      t.Addr,
			"target_node": metadata.RemoteNode,
			"target_pod":  metadata.RemotePod,
			"target_type": metadata.TargetType,
			"local_node":  metadata.LocalNode,
			"local_pod":   metadata.LocalPod,
		}).Observe(latency.Seconds())
		if resp.Hangup {
			// Remote indicated they're hanging up.  We're done.
			log.Ctx(ctx).Info().Msg("remote signalled hangup; closing connection")
			return
		}

		// Wait for next interval.
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
