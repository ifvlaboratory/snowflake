package snowflake_client

import (
	"io"
	"net"
	"time"
)

type packetConnWrapper struct {
	io.ReadWriter
	remoteAddr net.Addr
	localAddr  net.Addr
}

func (pcw *packetConnWrapper) ReadFrom(p []byte) (n int, addr net.Addr, err error) {
	n, err = pcw.Read(p)
	if err != nil {
		return 0, nil, err
	}
	return n, pcw.remoteAddr, nil
}

func (pcw *packetConnWrapper) WriteTo(p []byte, addr net.Addr) (n int, err error) {
	return pcw.Write(p)
}

func (pcw *packetConnWrapper) Close() error {
	return nil
}

func (pcw *packetConnWrapper) LocalAddr() net.Addr {
	return pcw.localAddr
}

func (pcw *packetConnWrapper) SetDeadline(t time.Time) error {
	return nil
}

func (pcw *packetConnWrapper) SetReadDeadline(t time.Time) error {
	return nil
}

func (pcw *packetConnWrapper) SetWriteDeadline(t time.Time) error {
	return nil
}
