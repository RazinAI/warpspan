// Package server accepts connections from libwarpspan.so on a unix
// SOCK_SEQPACKET socket and feeds decoded messages into a Collector.
package server

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/zraisan/warpspan/agent/collect"
	"github.com/zraisan/warpspan/agent/wire"
)

// Listen creates the socket at path, replacing a stale socket file. The socket
// is made world-writable because injected apps usually run as other users.
func Listen(path string) (*net.UnixListener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create socket dir: %w", err)
	}
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&fs.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove stale socket: %w", err)
		}
	}
	l, err := net.ListenUnix("unixpacket", &net.UnixAddr{Name: path, Net: "unixpacket"})
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o777); err != nil {
		l.Close()
		return nil, fmt.Errorf("chmod socket: %w", err)
	}
	return l, nil
}

// Server reads from connected capture libraries.
type Server struct {
	Collector *collect.Collector
	Log       *slog.Logger

	nextID atomic.Uint64
	wg     sync.WaitGroup
}

// Serve accepts connections until ctx is cancelled or the listener fails.
func (s *Server) Serve(ctx context.Context, l *net.UnixListener) error {
	go func() {
		<-ctx.Done()
		l.Close()
	}()
	for {
		conn, err := l.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				s.wg.Wait()
				return nil
			}
			return err
		}
		id := s.nextID.Add(1)
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handle(ctx, id, conn)
		}()
	}
}

func (s *Server) handle(ctx context.Context, id uint64, conn *net.UnixConn) {
	defer conn.Close()
	go func() {
		<-ctx.Done()
		conn.Close()
	}()

	log := s.Log.With("conn", id)
	buf := make([]byte, 2*wire.MaxMessage)
	var proc *collect.Process
	defer func() {
		s.Collector.Close(id)
		if proc != nil {
			log.Info("process disconnected", "pid", proc.PID)
		}
	}()

	for {
		n, _, flags, _, err := conn.ReadMsgUnix(buf, nil)
		if err != nil || n == 0 {
			return // EOF: the process exited
		}
		if flags&syscall.MSG_TRUNC != 0 {
			log.Warn("message truncated; closing connection", "bytes", n)
			return
		}
		msg, err := wire.Decode(buf[:n])
		if err != nil {
			log.Warn("bad message; closing connection", "err", err)
			return
		}
		switch m := msg.(type) {
		case *wire.Hello:
			proc = s.Collector.Hello(id, m)
			log.Info("process connected", "pid", m.PID, "service.name", proc.Resource[0].Value)
		case *wire.Kernels:
			if proc == nil {
				log.Warn("kernels before hello; closing connection")
				return
			}
			s.Collector.Add(id, m)
		}
	}
}
