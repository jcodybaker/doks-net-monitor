package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/rs/zerolog/log"
)

func EchoServer(ctx context.Context, addr string) error {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("opening listener: %w", err)
	}
	var wg sync.WaitGroup
	for ctx.Err() == nil {
		conn, err := l.Accept()
		if err != nil {
			log.Err(err).Msg("accepting connection")
			continue
		}

		log := log.Ctx(ctx)
		log.Debug().Msg("accepting connection")
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer conn.Close()
			if _, err := io.Copy(conn, conn); err != nil {
				log.Err(err).
					Str("remote_addr", conn.RemoteAddr().String()).
					Msg("proxying")
			}
			log.Debug().Msg("closing connection")
		}()
	}
	wg.Wait()
	return nil
}
