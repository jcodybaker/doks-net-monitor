package dnsecho

import (
	"context"
	"net"
	"sync"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"
)

type Server struct {
	s          *dns.Server
	wg         sync.WaitGroup
	missing    *prometheus.CounterVec
	outOfOrder *prometheus.CounterVec
	received   *prometheus.CounterVec
	mu         sync.Mutex
	lastID     map[string]uint16
}

func NewServer(addr string) *Server {
	if addr == "" {
		addr = ":53"
	}
	s := &Server{
		s: &dns.Server{Addr: addr, Net: "udp"},
		missing: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "dns_echo_server",
			Name:      "missing_requests_total",
			Help:      "Total number of missing requests",
		}, []string{"q"}),
		outOfOrder: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "dns_echo_server",
			Name:      "out_of_order_requests_total",
			Help:      "Total number of out of order requests",
		}, []string{"q"}),
		received: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "dns_echo_server",
			Name:      "received_requests_total",
			Help:      "Total number of received requests",
		}, []string{"q"}),
		lastID: make(map[string]uint16),
	}
	s.s.Handler = s
	return s
}

func (s *Server) Start() error {
	var err error
	s.s.PacketConn, err = net.ListenPacket("udp", s.s.Addr)
	if err != nil {
		return err
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		if err := s.s.ActivateAndServe(); err != nil {
			panic(err)
		}
	}()
	return nil
}

func (s *Server) Stop(ctx context.Context) error {
	err := s.s.ShutdownContext(ctx)
	s.wg.Wait()
	return err
}

func (m *Server) Register(reg prometheus.Registerer) error {
	if err := reg.Register(m.missing); err != nil {
		return err
	}
	if err := reg.Register(m.outOfOrder); err != nil {
		return err
	}
	if err := reg.Register(m.received); err != nil {
		return err
	}
	return nil
}

func (s *Server) ServeDNS(w dns.ResponseWriter, r *dns.Msg) {
	if r.Question == nil || len(r.Question) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	id := r.MsgHdr.Id
	q := r.Question[0].Name
	s.received.WithLabelValues(q).Inc()

	lastID := s.lastID[q]
	s.lastID[q] = id

	respHead := dns.RR_Header{
		Name:  r.Question[0].Name,
		Class: dns.ClassINET,
		Ttl:   uint32(lastID),
	}

	reply := new(dns.Msg)
	reply.SetReply(r)
	reply.SetRcode(r, dns.RcodeSuccess)
	var ip net.IP
	if uAddr, ok := w.RemoteAddr().(*net.UDPAddr); ok {
		ip = uAddr.IP
	} else if tAddr, ok := w.RemoteAddr().(*net.TCPAddr); ok {
		ip = tAddr.IP
	}
	if ip != nil {
		if v4Addr := ip.To4(); v4Addr != nil {
			respHead.Rrtype = dns.TypeA
			reply.Answer = append(reply.Answer, &dns.A{
				Hdr: respHead,
				A:   v4Addr,
			})
		} else if v6Addr := ip.To16(); v6Addr != nil {
			respHead.Rrtype = dns.TypeAAAA
			reply.Answer = append(reply.Answer, &dns.AAAA{
				Hdr:  respHead,
				AAAA: v6Addr,
			})
		}
	} else {
		respHead.Rrtype = dns.TypeTXT
		reply.Answer = append(reply.Answer, &dns.TXT{
			Hdr: respHead,
			Txt: []string{w.RemoteAddr().String()},
		})
	}

	diff, outOfOrder := diffWithOverflow(id, lastID)
	if outOfOrder {
		s.outOfOrder.WithLabelValues(q).Inc()
	} else if diff > 1 {
		s.missing.WithLabelValues(q).Add(float64(diff - 1))
	}

	w.WriteMsg(reply)
}

func diffWithOverflow(id, lastID uint16) (diff uint16, outOfOrder bool) {
	if id > lastID {
		diff = id - lastID
		if diff > 32768 {
			return 0, true
		}
		return diff, false
	}
	diff = lastID - id
	if diff < 32768 {
		return 0, true
	}
	return uint16(int(65535)-int(diff)) + 1, false
}
