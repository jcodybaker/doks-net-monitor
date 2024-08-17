package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"time"

	"github.com/google/uuid"
)

var (
	dialer = &net.Dialer{
		Timeout:   5 * time.Second,
		KeepAlive: 5 * time.Second,
	}
)

type TCPTarget struct {
	Addr string
	uuid string
}

type Payload struct {
	UUID   string `json:"uuid"`
	Hangup bool   `json:"hangup,omitempty"`
}

func NewTCPTarget(addr string) *TCPTarget {
	return &TCPTarget{
		Addr: addr,
		uuid: uuid.New().String(),
	}
}

func (t *TCPTarget) Run(ctx context.Context) error {
	for ctx.Err() == nil {
		conn, err := dialer.DialContext(ctx, "tcp", t.Addr)
		if err != nil {
			if ctx.Err() != nil {
				// If the parent ctx is cancelled we should ignore whatever error dial gives us.
				return nil
			}
			// TODO tag metric
			continue
		}
		t.probe(ctx, conn)
	}
	return nil
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
					// Clean hangup from remote!
					return
				}
				// Tag write error
			}
			return
		}

		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				// TODO tag err
				return
			}
			// Scanner.Err eats EOF assuming its just the end of the input, but we should never see an EOF w/o a hangup message
			// TODO tag unexpected EOF
			return
		}

		if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
			// TODO tag unexpected data error
			return
		}
		if resp.Hangup {
			// Remote indicated they're hanging up.  We're done.
			return
		}

		// TODO tag success w/ latency.
		latency := time.Since(start)

		// Wait for next interval.
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
