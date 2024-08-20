package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"

	"github.com/rs/zerolog/log"
)

func StartEchoServer(ctx context.Context, addr, nodeName, podName string) (stop func(), err error) {
	ll := log.Ctx(ctx).With().Str("component", "echoserver").Logger()
	ctx = ll.WithContext(ctx)
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("opening listener: %w", err)
	}
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	go func() {
		defer wg.Done()
		defer cancel()
		runEchoServer(ctx, l, &wg, nodeName, podName)
	}()
	return func() {
		if err := l.Close(); err != nil {
			ll.Warn().Err(err).Msg("closing listener")
		}
		cancel()
		wg.Wait()
	}, nil
}

func runEchoServer(ctx context.Context, l net.Listener, wg *sync.WaitGroup, nodeName, podName string) error {
	for ctx.Err() == nil {
		conn, err := l.Accept()
		if err != nil {
			log.Ctx(ctx).Err(err).Msg("accepting connection")
			continue
		}

		ll := log.Ctx(ctx).With().
			Str("remote_addr", conn.RemoteAddr().String()).
			Str("local_addr", conn.LocalAddr().String()).Logger()
		ll.Info().Msg("accepting connection")
		wg.Add(2)
		payloadChan := make(chan *Payload)
		go func() {
			// receive thread
			defer wg.Done()
			defer close(payloadChan)
			decoder := json.NewDecoder(conn)

			for ctx.Err() == nil {
				payload := &Payload{}
				if err := decoder.Decode(&payload); err != nil {
					if errors.Is(err, io.EOF) || errors.Is(err, os.ErrClosed) {
						ll.Info().Msg("connection closed")
						return
					}
					ll.Err(err).Msg("error decoding payload; closing connection")
					return
				}
				payloadChan <- payload
			}
			ll.Debug().Msg("stopping receive thread: shutting down")
		}()
		go func() {
			// send thread
			defer wg.Done()
			defer func() {
				ll.Debug().Msg("closing connection")
				conn.Close()
			}()

			var payload *Payload
			encoder := json.NewEncoder(conn)
		sendLoop:
			for ctx.Err() == nil {
				select {
				case payload = <-payloadChan:
					if payload == nil {
						// We'll get a nil payload when the channel is closed indicating the receive thread has exited.
						// This could indicate that the ctx has been cancelled or that the connection was closed.
						break sendLoop
					}
					if ctx.Err() != nil {
						payload.Hangup = true
						ll.Debug().Msg("sending hangup payload: shutdown signaled")
					}
					payload.NodeName = nodeName
					payload.PodName = podName
					if err := encoder.Encode(payload); err != nil {
						ll.Err(err).Msg("error echoing payload; closing connection")
						return
					}
					if payload.Hangup {
						return // We opportunitistically sent a hangup with the last response. Our work here is done.
					}
				case <-ctx.Done():
					// Returning here will close the connection which should unwedge the receive thread.
					break sendLoop
				}
			}
			// The ctx has been cancelled indicating the server is shutting down. Send a hangup so the remote understands
			// this is expected.
			ll.Debug().Msg("sending hangup payload: shutdown signaled")
			if err := encoder.Encode(&Payload{Hangup: true, NodeName: nodeName, PodName: podName}); err != nil && !errors.Is(err, os.ErrClosed) {
				ll.Err(err).Msg("sending hanging up payload")
			}
		}()
	}
	return nil
}
