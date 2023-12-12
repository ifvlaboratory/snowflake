package snowflake_server

import (
	"errors"
	"net"

	"gitlab.torproject.org/tpo/anti-censorship/pluggable-transports/snowflake/v2/common/turbotunnel"
)

type ConnID = turbotunnel.ClientID

type packetConnIDConnServer struct {
	// This net.Conn must preserve message boundaries.
	net.Conn
	connID           ConnID
	clientIDReceived bool
}

var ErrClientIDNotReceived = errors.New("ClientID not received")

func (p *packetConnIDConnServer) GetClientID() (ConnID, error) {
	if !p.clientIDReceived {
		return p.connID, ErrClientIDNotReceived
	}
	return p.connID, nil
}

func (p *packetConnIDConnServer) Read(buf []byte) (n int, err error) {
	n, err = p.Conn.Read(buf)
	if err != nil {
		return
	}
	switch buf[0] {
	case 0xfe:
		p.clientIDReceived = true
		copy(p.connID[:], buf[1:9])
		copy(buf[0:], buf[9:])
		return n - 9, nil
	case 0xff:
		copy(buf[0:], buf[1:])
		return n - 1, nil
	}
	return 0, nil
}

func (p *packetConnIDConnServer) Write(buf []byte) (n int, err error) {
	n, err = p.Conn.Write(append([]byte{0xff}, buf...))
	if err != nil {
		return 0, err
	}
	return len(buf) - 1, nil
}
