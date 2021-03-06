package lib

import (
	"compress/flate"
	"context"
	"io"
	"net"

	"github.com/golang/snappy"
	"github.com/pkg/errors"
)

// WrapTransCompression wraps a Transport with a given compression method.
func WrapTransCompression(inner Transport, method string) (Transport, error) {
	switch method {
	case "snappy", "deflate":
		return &compTransWrapper{inner, method}, nil
	default:
		return nil, errors.New("unknown compression method: " + method)
	}
}

type compTransWrapper struct {
	inner  Transport
	method string
}

func (w *compTransWrapper) Dial(
	ctx context.Context, address string) (net.Conn, error) {
	conn, err := w.inner.Dial(ctx, address)
	if err == nil {
		conn, err = compWrapConn(conn, w.method)
	}
	return conn, err
}

func (w *compTransWrapper) Listen(address string) (net.Listener, error) {
	listener, err := w.inner.Listen(address)
	if err == nil {
		listener = &compListenerWrapper{Listener: listener, method: w.method}
	}
	return listener, err
}

type compConnWrapper struct {
	net.Conn
	compReader io.Reader
	compWriter writeCloseFlusher
}

type compConnWithPeerIDs struct {
	*compConnWrapper
}

func (w *compConnWithPeerIDs) GetPeerIdentifiers() ([]*PeerIdentifier, error) {
	return w.Conn.(WithPeerIdentifiers).GetPeerIdentifiers()
}

func compWrapConn(inner net.Conn, method string) (net.Conn, error) {
	var wrapper *compConnWrapper
	switch method {
	case "snappy":
		wrapper = &compConnWrapper{
			inner, snappy.NewReader(inner), snappy.NewBufferedWriter(inner)}
	case "deflate":
		w, e := flate.NewWriter(inner, flate.DefaultCompression)
		if e != nil {
			return nil, errors.WithStack(e)
		}
		wrapper = &compConnWrapper{inner, flate.NewReader(inner), w}
	default:
		return nil, errors.New("unknown compression method: " + method)
	}

	if _, withPIDs := inner.(WithPeerIdentifiers); withPIDs {
		return &compConnWithPeerIDs{wrapper}, nil
	}
	return wrapper, nil
}

func (w *compConnWrapper) Read(b []byte) (int, error) {
	return w.compReader.Read(b)
}

func (w *compConnWrapper) Write(b []byte) (int, error) {
	n, err := w.compWriter.Write(b)
	if err == nil {
		err = w.compWriter.Flush()
	}
	return n, err
}

func (w *compConnWrapper) Close() (err error) {
	err = w.compWriter.Close()
	if err == nil {
		err = w.Conn.Close()
	} else {
		_ = w.Conn.Close()
	}
	return
}

type compListenerWrapper struct {
	net.Listener
	method string
}

func (w *compListenerWrapper) Accept() (net.Conn, error) {
	conn, err := w.Listener.Accept()
	if err == nil {
		conn, err = compWrapConn(conn, w.method)
	}
	return conn, err
}

type writeCloseFlusher interface {
	io.WriteCloser
	Flush() error
}
