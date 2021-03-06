package lib

import (
	"container/list"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pkg/errors"
	"github.com/xtaci/kcp-go"
)

// KCPTransport is a connection-aware Transport based on the KCP protocol.
// Closing a connection will notify the peer end on a best-efforts basis.
type KCPTransport struct {
	noDelay           int
	interval          int
	resend            int
	nc                int
	sndWnd            int
	rcvWnd            int
	dataShards        int
	parityShards      int
	keepAliveInterval time.Duration
	keepAliveTimeout  time.Duration

	conns    *list.List
	connsMtx sync.Mutex
}

// kcpCloseSendTimeout is the timeout for sending the kcpClose signal
// when closing a connection. This is a variable so that it can be altered
// in tests, but it should be considered as a constant in the production code.
var kcpCloseSendTimeout = time.Second * 10

var kcpCloseLingerTimeout = time.Second * 10

// NewKCPTransport creates KCPTransport with a given configuration.
func NewKCPTransport(config KCPConfig) (*KCPTransport, error) {
	// var transport *KCPTransport
	t := new(KCPTransport)
	switch config.Mode {
	case "", "normal":
		t.noDelay, t.interval, t.resend, t.nc = 0, 25, 0, 0
	case "fast":
		t.noDelay, t.interval, t.resend, t.nc = 0, 25, 2, 1
	case "fast2":
		t.noDelay, t.interval, t.resend, t.nc = 1, 10, 2, 1
	default:
		return nil, errors.New("invalid KCP mode: " + config.Mode)
	}

	switch config.Optimize {
	case "", "balance":
		t.sndWnd, t.rcvWnd = 256, 256
	case "receive":
		t.sndWnd, t.rcvWnd = 128, 512
	case "send":
		t.sndWnd, t.rcvWnd = 512, 128
	case "server":
		t.sndWnd, t.rcvWnd = 1024, 1024
	case "_test_small":
		t.sndWnd, t.rcvWnd = 32, 32
	default:
		return nil, errors.New("invalid optimization: " + config.Optimize)
	}

	if config.FEC {
		if config.FECDist == "" {
			t.dataShards = 10
			t.parityShards = 2
		} else {
			_, err := fmt.Sscanf(
				config.FECDist, "%d,%d", &t.dataShards, &t.parityShards)
			if err != nil {
				return nil, errors.Wrap(err, "invalid FEC distribution")
			}
		}
	}

	if (config.KeepAliveInterval == "") != (config.KeepAliveTimeout == "") {
		return nil, errors.New(
			"'keep_alive_interval' must be used with 'keep_alive_timeout'")
	}
	if config.KeepAliveInterval != "" {
		var err error
		t.keepAliveInterval, err = time.ParseDuration(config.KeepAliveInterval)
		if err != nil || t.keepAliveInterval <= 0 {
			return nil, errors.New("invalid 'keep_alive_interval'")
		}
		t.keepAliveTimeout, err = time.ParseDuration(config.KeepAliveTimeout)
		if err != nil || t.keepAliveTimeout <= 0 {
			return nil, errors.New("invalid 'keep_alive_timeout'")
		}
		t.conns = list.New()
		go t.runKeepAliveManager()
	} else {
		t.conns = nil
	}

	return t, nil
}

// Dial creates a KCP connection to a remote host.
func (t *KCPTransport) Dial(
	ctx context.Context, address string) (net.Conn, error) {
	type result struct {
		conn net.Conn
		err  error
	}

	resultCh := make(chan result, 1)

	go func() {
		kcpConn, err := kcp.DialWithOptions(
			address, nil, t.dataShards, t.parityShards)
		if err != nil {
			resultCh <- result{nil, err}
		} else {
			resultCh <- result{t.wrapKCPConn(kcpConn), nil}
		}
	}()

	select {
	case rst := <-resultCh:
		if rst.err != nil {
			return nil, errors.WithStack(rst.err)
		}
		return rst.conn, nil
	case <-ctx.Done():
		return nil, errors.WithStack(ctx.Err())
	}
}

// Listen creates a KCP listener on a given address.
func (t *KCPTransport) Listen(address string) (net.Listener, error) {
	listener, err := kcp.ListenWithOptions(
		address, nil, t.dataShards, t.parityShards)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	return &kcpListenerWrapper{listener, t}, nil
}

func (t *KCPTransport) runKeepAliveManager() {
	// kill the process if this goroutine panics to avoid misbehaviour
	defer func() {
		if err := recover(); err != nil {
			_, _ = fmt.Fprintf(
				os.Stderr, "KCPTransport KeepAliveManager crashed: %#v", err)
			os.Exit(1)
		}
	}()

	ticker := time.NewTicker(t.keepAliveInterval / 4)
	timeout := t.keepAliveTimeout.Nanoseconds()
	interval := t.keepAliveInterval.Nanoseconds()
	for {
		now := (<-ticker.C).UnixNano()
		t.connsMtx.Lock()
		for e := t.conns.Front(); e != nil; {
			next := e.Next()
			conn := e.Value.(*kcpConnWrapper)
			lastSend := atomic.LoadInt64(&conn.lastSend)
			lastReadStart := atomic.LoadInt64(&conn.lastReadStart)
			lastWriteStart := atomic.LoadInt64(&conn.lastWriteStart)
			if lastSend == 0 { // closed
				t.conns.Remove(e)
			} else if lastReadStart > 0 && now-lastReadStart > timeout {
				// read time out, lost
				t.conns.Remove(e)
				go conn.Close() // nolint: errcheck
			} else if lastWriteStart > 0 && now-lastWriteStart > timeout {
				// write time out, lost
				t.conns.Remove(e)
				go conn.Close() // nolint: errcheck
			} else if now-lastSend > interval { // long idle
				go conn.sendKeepAlive()
			}
			e = next
		}
		t.connsMtx.Unlock()
	}
}

type kcpConnWrapper struct {
	*kcp.UDPSession
	rdMtx      sync.Mutex
	rdDataLeft uint32

	// UNIX ns time of last send time, 0 indicates the conn was closed
	lastSend int64
	// UNIX ns time of the start time of last read operation.
	lastReadStart int64
	// UNIX ns time of the start time of last write operation.
	lastWriteStart int64
}

const (
	kcpDataPacket = 0
	kcpClose      = 1
	kcpKeepAlive  = 2
)

func (t *KCPTransport) wrapKCPConn(kcpConn *kcp.UDPSession) *kcpConnWrapper {
	kcpConn.SetNoDelay(t.noDelay, t.interval, t.resend, t.nc)
	kcpConn.SetStreamMode(true)
	kcpConn.SetWindowSize(t.sndWnd, t.rcvWnd)
	wrapped := new(kcpConnWrapper)
	wrapped.UDPSession = kcpConn
	wrapped.rdDataLeft = 0
	wrapped.lastSend = time.Now().UnixNano()
	wrapped.lastReadStart = 0
	wrapped.lastWriteStart = 0

	if t.conns != nil {
		t.connsMtx.Lock()
		defer t.connsMtx.Unlock()
		t.conns.PushBack(wrapped)
	}
	return wrapped
}

func (c *kcpConnWrapper) Read(b []byte) (int, error) {
	c.rdMtx.Lock()
	defer c.rdMtx.Unlock()
	for c.rdDataLeft == 0 {
		var header [4]byte
		if _, err := c.read(header[:1]); err != nil {
			return 0, err
		}
		switch header[0] {
		case kcpClose:
			atomic.StoreInt64(&c.lastSend, 0)
			return 0, io.EOF
		case kcpDataPacket:
			if _, err := c.read(header[:]); err != nil {
				return 0, err
			}
			// network byte order
			c.rdDataLeft = binary.BigEndian.Uint32(header[:])
		case kcpKeepAlive:
			continue
		default:
			return 0, errors.Errorf("invalid KCP header %x", header[0])
		}
	}

	if len(b) > int(c.rdDataLeft) {
		b = b[:c.rdDataLeft]
	}
	n, err := c.read(b)
	if err != nil {
		return 0, err
	}
	c.rdDataLeft -= uint32(n)
	return n, nil
}

func (c *kcpConnWrapper) Write(b []byte) (int, error) {
	if len(b) > 0xffffffff {
		return 0, errors.New("send buffer size exceeds limitation")
	}
	n := uint32(len(b))
	buf := GlobalBufPool.Get(uint(n + 5))
	defer GlobalBufPool.Free(buf)
	buf[0] = kcpDataPacket
	binary.BigEndian.PutUint32(buf[1:5], n)
	copy(buf[5:], b)

	atomic.StoreInt64(&c.lastSend, time.Now().UnixNano())
	atomic.StoreInt64(&c.lastWriteStart, time.Now().UnixNano())
	defer atomic.StoreInt64(&c.lastWriteStart, 0)
	return c.UDPSession.Write(buf)
}

func (c *kcpConnWrapper) Close() error {
	atomic.StoreInt64(&c.lastSend, 0) // indicate the conn is closed
	_ = c.UDPSession.SetWriteDeadline(time.Now().Add(kcpCloseSendTimeout))
	_, _ = c.UDPSession.Write([]byte{kcpClose})
	go func() {
		time.Sleep(kcpCloseLingerTimeout)
		c.UDPSession.Close()
	}()
	return nil
}

func (c *kcpConnWrapper) sendKeepAlive() {
	atomic.StoreInt64(&c.lastSend, time.Now().UnixNano())
	if _, err := c.UDPSession.Write([]byte{kcpKeepAlive}); err != nil {
		_ = c.Close()
	}
}

func (c *kcpConnWrapper) read(b []byte) (int, error) {
	defer atomic.StoreInt64(&c.lastReadStart, 0)
	atomic.StoreInt64(&c.lastReadStart, time.Now().UnixNano())
	return c.UDPSession.Read(b)
}

type kcpListenerWrapper struct {
	*kcp.Listener
	kcpTransport *KCPTransport
}

func (l *kcpListenerWrapper) Accept() (net.Conn, error) {
	conn, err := l.Listener.AcceptKCP()
	if err != nil {
		return nil, err
	}
	return l.kcpTransport.wrapKCPConn(conn), nil
}

func (l *kcpListenerWrapper) AcceptKCP() (*kcp.UDPSession, error) {
	panic("should use kcpListenerWrapper.Accept() instead")
}

func (l *kcpListenerWrapper) Close() error {
	err := l.Listener.Close()
	return err
}
