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
)

var (
	dialer = &net.Dialer{
		Timeout:   5 * time.Second,
		KeepAlive: 5 * time.Second,
	}
)

type TCPMetrics struct {
	ConnectCount     *prometheus.CounterVec
	ProbeCount       *prometheus.CounterVec
	ProbeLatencyHist *prometheus.HistogramVec
}

func NewTCPMetrics(nodeName string) *TCPMetrics {
	return &TCPMetrics{
		ConnectCount: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "tcp_persistent_prober",
			Subsystem: "connect",
			Name:      "count",
			Help:      "Total count of connect attempts by outcome.",
			ConstLabels: prometheus.Labels{
				"node": nodeName,
			},
		}, []string{"target", "outcome", "error"}),
		ProbeCount: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "tcp_persistent_prober",
			Subsystem: "probe",
			Name:      "count",
			Help:      "Total count of probes by outcome.",
			ConstLabels: prometheus.Labels{
				"node": nodeName,
			},
		}, []string{"target", "outcome", "error"}),
		ProbeLatencyHist: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace: "tcp_persistent_prober",
				Subsystem: "probe",
				Name:      "latency",
				Help:      "Latency of probes.",
				ConstLabels: prometheus.Labels{
					"node": nodeName,
				},
				Buckets: prometheus.DefBuckets,
			}, []string{"target"},
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

type TCPTarget struct {
	Addr string
	uuid string
	*TCPMetrics
	stop  context.CancelFunc
	mutex sync.Mutex
}

type Payload struct {
	UUID   string `json:"uuid"`
	Hangup bool   `json:"hangup,omitempty"`
}

func NewTCPTarget(addr string, m *TCPMetrics) *TCPTarget {
	return &TCPTarget{
		Addr:       addr,
		uuid:       uuid.New().String(),
		TCPMetrics: m,
	}
}

func (t *TCPTarget) Run(ctx context.Context) {
	t.mutex.Lock()
	if t.stop != nil {
		log.Ctx(ctx).Warn().Msg("tcp target already running")
		t.mutex.Unlock()
	}
	ctx, stop := context.WithCancel(ctx)
	t.stop = stop
	t.mutex.Unlock()
	defer func() {
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
				return
			}
			t.ConnectCount.With(prometheus.Labels{
				"target":  t.Addr,
				"outcome": "error",
				"error":   err.Error(),
			}).Inc()
			continue
		}
		t.ConnectCount.With(prometheus.Labels{
			"target":  t.Addr,
			"outcome": "success",
			"error":   "",
		}).Inc()
		t.probe(ctx, conn)
	}
	return
}

func (t *TCPTarget) Stop() {
	t.mutex.Lock()
	if t.stop != nil {
		t.stop()
	}
	t.mutex.Unlock()
}

func (t *TCPTarget) probe(ctx context.Context, conn net.Conn) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	defer conn.Close()
	j := json.NewEncoder(conn)
	payload := &Payload{UUID: t.uuid}
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
					// TODO - Sleep here?
					return
				}
				t.ProbeCount.With(prometheus.Labels{
					"target":  t.Addr,
					"outcome": "write_error",
					"error":   err.Error(),
				}).Inc()
			}
			return
		}

		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				t.ProbeCount.With(prometheus.Labels{
					"target":  t.Addr,
					"outcome": "read_error",
					"error":   err.Error(),
				}).Inc()
				return
			}
			// Scanner.Err eats EOF assuming its just the end of the input, but we should never see an EOF w/o a hangup message
			t.ProbeCount.With(prometheus.Labels{
				"target":  t.Addr,
				"outcome": "read_error",
				"error":   io.EOF.Error(),
			}).Inc()
			return
		}

		if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
			t.ProbeCount.With(prometheus.Labels{
				"target":  t.Addr,
				"outcome": "read_error",
				"error":   fmt.Sprintf("parse error: %v", err),
			}).Inc()
			return
		}
		if resp.Hangup {
			// Remote indicated they're hanging up.  We're done.
			return
		}

		latency := time.Since(start)
		t.ProbeCount.With(prometheus.Labels{
			"target":  t.Addr,
			"outcome": "success",
			"error":   "",
		}).Inc()
		t.ProbeLatencyHist.With(prometheus.Labels{"target": t.Addr}).Observe(latency.Seconds())

		// Wait for next interval.
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
