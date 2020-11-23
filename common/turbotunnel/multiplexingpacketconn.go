package turbotunnel

import (
	"container/list"
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// MultiplexingPacketConn implements a long-lived net.PacketConn atop a sequence of
// other, transient net.PacketConns. MultiplexingPacketConn creates a new
// net.PacketConn by calling a provided dialContext function. Whenever the
// net.PacketConn experiences a ReadFrom or WriteTo error, MultiplexingPacketConn
// calls the dialContext function again and starts sending and receiving packets
// on the new net.PacketConn. MultiplexingPacketConn's own ReadFrom and WriteTo
// methods return an error only when the dialContext function returns an error.
//
// MultiplexingPacketConn uses static local and remote addresses that are independent
// of those of any dialed net.PacketConn.
type MultiplexingPacketConn struct {
	localAddr   net.Addr
	remoteAddr  net.Addr
	dialContext func(context.Context) (net.PacketConn, error)
	recvQueue   chan []byte
	sendQueue   chan []byte
	closed      chan struct{}
	closeOnce   sync.Once
	// The first dial error, which causes the clientPacketConn to be
	// closed and is returned from future read/write operations. Compare to
	// the rerr and werr in io.Pipe.
	err atomic.Value
	// The number of snowflakes we multiplex across
	count  uint
	queues *list.List
}

type Peer struct {
	net.PacketConn
	sendQueue chan []byte
}

// NewQueuePacketConn makes a new MultiplexingPacketConn, with the given static local
// and remote addresses, count, and dialContext function.
func NewMultiplexingPacketConn(
	localAddr, remoteAddr net.Addr, count uint,
	dialContext func(context.Context) (net.PacketConn, error),
) *MultiplexingPacketConn {
	c := &MultiplexingPacketConn{
		localAddr:   localAddr,
		remoteAddr:  remoteAddr,
		dialContext: dialContext,
		recvQueue:   make(chan []byte, queueSize),
		sendQueue:   make(chan []byte, queueSize),
		closed:      make(chan struct{}),
		err:         atomic.Value{},
		count:       count,
		queues:      list.New(),
	}
	go c.dialLoop()
	return c
}

// dialLoop repeatedly calls c.dialContext and passes the resulting
// net.PacketConn to c.exchange. It returns only when c is closed or dialContext
// returns an error.
func (c *MultiplexingPacketConn) dialLoop() {
	ctx, cancel := context.WithCancel(context.Background())

	// Create a sendQueue for each potential peer
	tokens := make(chan chan []byte, c.count)
	for i := uint(0); i < c.count; i++ {
		queue := make(chan []byte, queueSize)
		c.queues.PushBack(queue)
		tokens <- queue
	}

	errChan := make(chan struct{})
	defer close(errChan)

	go c.multiplex(errChan)
	for {
		select {
		case <-c.closed:
			cancel()
			return
		default:
		}
		queue := <-tokens
		go func() {
			conn, err := c.dialContext(ctx)
			if err != nil {
				c.closeWithError(err)
				cancel()
				return
			}
			p := &Peer{PacketConn: conn, sendQueue: queue}
			c.exchange(p)
			conn.Close()
			tokens <- queue
		}()
	}
}

// multiplex packets received from c.sendQueue to WebRTC connections
// in the list of Peers
// currently uses a round-robin method of splitting traffic
func (c *MultiplexingPacketConn) multiplex(ch chan struct{}) {
	for {
		select {
		case <-ch:
			return
		case p := <-c.sendQueue:
			e := c.queues.Front()
			queue := e.Value.(chan []byte)
			queue <- p
			c.queues.MoveToBack(e)
		}
	}
}

// exchange calls ReadFrom on the given net.PacketConn and places the resulting
// packets in the receive queue, and takes packets from the send queue and calls
// WriteTo on them, making the current net.PacketConn active.
func (c *MultiplexingPacketConn) exchange(conn *Peer) {
	readErrCh := make(chan error)
	writeErrCh := make(chan error)

	go func() {
		defer close(readErrCh)
		for {
			select {
			case <-c.closed:
				return
			case <-writeErrCh:
				return
			default:
			}

			var buf [1500]byte
			n, _, err := conn.ReadFrom(buf[:])
			if err != nil {
				readErrCh <- err
				return
			}
			p := make([]byte, n)
			copy(p, buf[:])
			select {
			case c.recvQueue <- p:
			default: // OK to drop packets.
			}
		}
	}()

	go func() {
		defer close(writeErrCh)
		for {
			select {
			case <-c.closed:
				return
			case <-readErrCh:
				return
			case p := <-conn.sendQueue:
				_, err := conn.WriteTo(p, c.remoteAddr)
				if err != nil {
					writeErrCh <- err
					return
				}
			}
		}
	}()

	select {
	case <-readErrCh:
	case <-writeErrCh:
	}
}

// ReadFrom reads a packet from the currently active net.PacketConn. The
// packet's original remote address is replaced with the MultiplexingPacketConn's own
// remote address.
func (c *MultiplexingPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	select {
	case <-c.closed:
		return 0, nil, &net.OpError{Op: "read", Net: c.LocalAddr().Network(), Source: c.LocalAddr(), Addr: c.remoteAddr, Err: c.err.Load().(error)}
	default:
	}
	select {
	case <-c.closed:
		return 0, nil, &net.OpError{Op: "read", Net: c.LocalAddr().Network(), Source: c.LocalAddr(), Addr: c.remoteAddr, Err: c.err.Load().(error)}
	case buf := <-c.recvQueue:
		return copy(p, buf), c.remoteAddr, nil
	}
}

// WriteTo writes a packet to the currently active net.PacketConn. The addr
// argument is ignored and instead replaced with the MultiplexingPacketConn's own
// remote address.
func (c *MultiplexingPacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	// addr is ignored.
	select {
	case <-c.closed:
		return 0, &net.OpError{Op: "write", Net: c.LocalAddr().Network(), Source: c.LocalAddr(), Addr: c.remoteAddr, Err: c.err.Load().(error)}
	default:
	}
	buf := make([]byte, len(p))
	copy(buf, p)
	select {
	case c.sendQueue <- buf:
		return len(buf), nil
	default:
		// Drop the outgoing packet if the send queue is full.
		return len(buf), nil
	}
}

// closeWithError unblocks pending operations and makes future operations fail
// with the given error. If err is nil, it becomes errClosedPacketConn.
func (c *MultiplexingPacketConn) closeWithError(err error) error {
	var once bool
	c.closeOnce.Do(func() {
		// Store the error to be returned by future read/write
		// operations.
		if err == nil {
			err = errors.New("operation on closed connection")
		}
		c.err.Store(err)
		close(c.closed)
		once = true
	})
	if !once {
		return &net.OpError{Op: "close", Net: c.LocalAddr().Network(), Addr: c.LocalAddr(), Err: c.err.Load().(error)}
	}
	return nil
}

// Close unblocks pending operations and makes future operations fail with a
// "closed connection" error.
func (c *MultiplexingPacketConn) Close() error {
	return c.closeWithError(nil)
}

// LocalAddr returns the localAddr value that was passed to NewMultiplexingPacketConn.
func (c *MultiplexingPacketConn) LocalAddr() net.Addr { return c.localAddr }

func (c *MultiplexingPacketConn) SetDeadline(t time.Time) error      { return errNotImplemented }
func (c *MultiplexingPacketConn) SetReadDeadline(t time.Time) error  { return errNotImplemented }
func (c *MultiplexingPacketConn) SetWriteDeadline(t time.Time) error { return errNotImplemented }
