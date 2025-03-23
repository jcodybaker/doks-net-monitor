package dnsecho

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jcodybaker/doks-net-monitor/pkg/types"
	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// DNSClientMetrics holds metrics roots for all probers in this process.
type DNSClientMetrics struct {
	probeOutcome     *prometheus.CounterVec
	txLoss           *prometheus.CounterVec
	dupe             *prometheus.CounterVec
	probeLatencyHist *prometheus.HistogramVec
}

// NewDNSClientMetrics creates a metrics root for DNSTarget probers.
func NewDNSClientMetrics() *DNSClientMetrics {
	return &DNSClientMetrics{
		probeOutcome: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "dns_echo_client",
			Name:      "requests_total",
			Help:      "Total number of requests arranged by outcome",
		}, []string{"target", "target_node", "target_pod", "target_type", "local_node", "local_pod", "outcome", "error"}),
		txLoss: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "dns_echo_client",
			Name:      "tx_loss_total",
			Help:      "Total number of requests transmitted but not received by the remote",
		}, []string{"target", "target_node", "target_pod", "target_type", "local_node", "local_pod"}),
		dupe: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "dns_echo_client",
			Name:      "duplicate_tx_total",
			Help:      "Total number of requests transmitted which arrived in duplicate at the remote",
		}, []string{"target", "target_node", "target_pod", "target_type", "local_node", "local_pod"}),
		probeLatencyHist: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace: "dns_echo_client",
				Name:      "received_requests_total",
				Help:      "Total number of received requests",
				Buckets:   prometheus.DefBuckets,
			}, []string{"target", "target_node", "target_pod", "target_type", "local_node", "local_pod"}),
	}
}

func (m *DNSClientMetrics) Register(reg prometheus.Registerer) error {
	if err := reg.Register(m.probeOutcome); err != nil {
		return err
	}
	if err := reg.Register(m.txLoss); err != nil {
		return err
	}
	if err := reg.Register(m.probeLatencyHist); err != nil {
		return err
	}
	if err := reg.Register(m.dupe); err != nil {
		return err
	}
	return nil
}

// DNSTarget connects to a DNS destination and queries on a regular interval, reporting statistics.
type DNSTarget struct {
	Addr string
	uuid string
	*DNSClientMetrics
	stop          context.CancelFunc
	mutex         sync.Mutex
	metadata      types.TargetMetadata
	log           zerolog.Logger
	dnsClient     *dns.Client
	probeID       uint16
	probeClientID string
	onSuccess     func()
	onError       func(err error)
	onTxLoss      func(diff uint16)
	onDupe        func()
}

// NewDNSTarget creates a new DNSTarget.
func NewDNSTarget(probeInterval time.Duration, addr string, m *DNSClientMetrics, metadata types.TargetMetadata) *DNSTarget {
	return &DNSTarget{
		Addr:             addr,
		uuid:             uuid.New().String(),
		DNSClientMetrics: m,
		metadata:         metadata,
		log: log.With().
			Str("component", "probe").
			Str("target", addr).
			Str("target_type", metadata.TargetType).
			Str("target_node", metadata.RemoteNode).
			Str("target_pod", metadata.RemotePod).
			Str("local_node", metadata.LocalNode).
			Str("local_pod", metadata.LocalPod).
			Logger(),
		dnsClient:     &dns.Client{},
		probeClientID: fmt.Sprintf("%s.%s.%s.", metadata.TargetType, metadata.LocalPod, metadata.LocalNode),
	}
}

func (t *DNSTarget) Run(ctx context.Context) {
	ctx = t.log.WithContext(ctx)
	t.mutex.Lock()
	if t.stop != nil {
		t.log.Warn().Msg("dns target already running")
		t.mutex.Unlock()
		return
	}
	ctx, stop := context.WithCancel(ctx)
	t.stop = stop
	t.mutex.Unlock()
	defer func() {
		t.log.Info().Msg("stopping probes")
		stop()
		t.mutex.Lock()
		t.stop = nil
		t.mutex.Unlock()
	}()

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for ctx.Err() == nil {
		t.probe(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (t *DNSTarget) probe(ctx context.Context) {
	t.probeID++
	req := &dns.Msg{
		MsgHdr: dns.MsgHdr{
			Id:               t.probeID,
			RecursionDesired: true,
		},
		Question: []dns.Question{
			{Name: t.probeClientID, Qtype: dns.TypeA, Qclass: dns.ClassINET},
		},
	}
	// t.log.Debug().
	// 	Uint16("probe_id", t.probeID).
	// 	Str("probe_client_id", t.probeClientID).
	// 	Msg("querying remote")
	resp, rtt, err := t.dnsClient.ExchangeContext(ctx, req, t.Addr)
	if err != nil {
		if ctx.Err() != nil {
			// If the parent ctx is cancelled we should ignore whatever error dial gives us.
			t.log.Info().Msg("cancelling probe: shutting down")
			return
		}
		t.probeOutcome.With(prometheus.Labels{
			"target":      t.Addr,
			"target_node": t.metadata.RemoteNode,
			"target_pod":  t.metadata.RemotePod,
			"target_type": t.metadata.TargetType,
			"local_node":  t.metadata.LocalNode,
			"local_pod":   t.metadata.LocalPod,
			"outcome":     "error",
			"error":       err.Error(),
		}).Inc()
		t.log.Err(err).Msg("querying remote")
		if t.onError != nil {
			t.onError(err)
		}
		return
	}
	if len(resp.Answer) != 1 {
		t.log.Warn().Msg("unexpected number of answers")
		t.probeOutcome.With(prometheus.Labels{
			"target":      t.Addr,
			"target_node": t.metadata.RemoteNode,
			"target_pod":  t.metadata.RemotePod,
			"target_type": t.metadata.TargetType,
			"local_node":  t.metadata.LocalNode,
			"local_pod":   t.metadata.LocalPod,
			"outcome":     "corrupt",
			"error":       "unexpected number of answers",
		}).Inc()
		return
	}
	if diff, _ := diffWithOverflow(t.probeID, uint16(resp.Answer[0].Header().Ttl)); diff > 1 {
		t.log.Warn().
			Uint16("diff", diff-1). // we only find about loss when a successful probe returns, sub 1 off for that packet
			Uint16("client_id", req.Id).
			Uint16("resp_last_id", uint16(resp.Answer[0].Header().Ttl)).
			Str("probe_client_id", t.probeClientID).
			Msg("tx loss detected")
		t.txLoss.With(prometheus.Labels{
			"target":      t.Addr,
			"target_node": t.metadata.RemoteNode,
			"target_pod":  t.metadata.RemotePod,
			"target_type": t.metadata.TargetType,
			"local_node":  t.metadata.LocalNode,
			"local_pod":   t.metadata.LocalPod,
		}).Add(float64(diff))
		if t.onTxLoss != nil {
			t.onTxLoss(diff)
		}
	} else if diff == 0 {
		t.log.Warn().
			Uint16("diff", diff).
			Uint16("client_id", req.Id).
			Uint16("resp_last_id", uint16(resp.Answer[0].Header().Ttl)).
			Str("probe_client_id", t.probeClientID).
			Msg("duplicate tx detected")
		t.dupe.With(prometheus.Labels{
			"target":      t.Addr,
			"target_node": t.metadata.RemoteNode,
			"target_pod":  t.metadata.RemotePod,
			"target_type": t.metadata.TargetType,
			"local_node":  t.metadata.LocalNode,
			"local_pod":   t.metadata.LocalPod,
		}).Inc()
		if t.onDupe != nil {
			t.onDupe()
		}
		return
	}
	t.probeOutcome.With(prometheus.Labels{
		"target":      t.Addr,
		"target_node": t.metadata.RemoteNode,
		"target_pod":  t.metadata.RemotePod,
		"target_type": t.metadata.TargetType,
		"local_node":  t.metadata.LocalNode,
		"local_pod":   t.metadata.LocalPod,
		"outcome":     "success",
		"error":       "",
	}).Inc()
	t.probeLatencyHist.With(prometheus.Labels{
		"target":      t.Addr,
		"target_node": t.metadata.RemoteNode,
		"target_pod":  t.metadata.RemotePod,
		"target_type": t.metadata.TargetType,
		"local_node":  t.metadata.LocalNode,
		"local_pod":   t.metadata.LocalPod,
	}).Observe(rtt.Seconds())
	if t.onSuccess != nil {
		t.onSuccess()
	}
}

func (t *DNSTarget) Stop() {
	t.mutex.Lock()
	if t.stop != nil {
		t.stop()
	}
	t.mutex.Unlock()
}
