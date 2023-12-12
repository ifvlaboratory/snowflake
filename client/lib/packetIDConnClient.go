package snowflake_client

import (
	"io"
	"log"
	"net"
	"time"

	"gitlab.torproject.org/tpo/anti-censorship/pluggable-transports/snowflake/v2/common/turbotunnel"
)

const (
	packetClientIDConn_StateNew = iota
	packetClientIDConn_StateConnectionIDAcknowledged
)

type ClientID = turbotunnel.ClientID

func newPacketClientIDConn(ClientID ClientID, transport io.ReadWriter) *packetClientIDConn {
	return &packetClientIDConn{
		state:     packetClientIDConn_StateNew,
		ConnID:    ClientID,
		transport: transport,
	}
}

type packetClientIDConn struct {
	state     int
	ConnID    ClientID
	transport io.ReadWriter
}

func (c *packetClientIDConn) Write(p []byte) (int, error) {
	switch c.state {
	case packetClientIDConn_StateConnectionIDAcknowledged:
		packet := make([]byte, len(p)+1)
		packet[0] = 0xff
		copy(packet[1:], p)
		_, err := c.transport.Write(packet)
		if err != nil {
			return 0, err
		}
		return len(p), nil
	case packetClientIDConn_StateNew:
		packet := make([]byte, len(p)+1+len(c.ConnID))
		packet[0] = 0xfe
		copy(packet[1:], c.ConnID[:])
		copy(packet[1+len(c.ConnID):], p)
		_, err := c.transport.Write(packet)
		if err != nil {
			return 0, err
		}
		return len(p), nil
	default:
		panic("invalid state")
	}
}

func (c *packetClientIDConn) Read(p []byte) (int, error) {
	n, err := c.transport.Read(p)
	if err != nil {
		return 0, err
	}
	if p[0] == 0xff {
		c.state = packetClientIDConn_StateConnectionIDAcknowledged
		return copy(p, p[1:n]), nil
	} else {
		log.Println("discarded unknown packet")
	}
	return 0, nil
}

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
