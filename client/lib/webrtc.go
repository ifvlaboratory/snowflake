package lib

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"sync"
	"time"

	"github.com/pion/webrtc/v2"
)

// Remote WebRTC peer.
//
// Handles preparation of go-webrtc PeerConnection. Only ever has
// one DataChannel.
type WebRTCPeer struct {
	id        string
	pc        *webrtc.PeerConnection
	transport *webrtc.DataChannel

	recvPipe    *io.PipeReader
	writePipe   *io.PipeWriter
	lastReceive time.Time
	buffer      bytes.Buffer

	closed bool

	lock sync.Mutex // Synchronization for DataChannel destruction
	once sync.Once  // Synchronization for PeerConnection destruction

	BytesLogger BytesLogger
}

// Construct a WebRTC PeerConnection.
func NewWebRTCPeer(config *webrtc.Configuration,
	broker *BrokerChannel) (*WebRTCPeer, error) {
	connection := new(WebRTCPeer)
	{
		var buf [8]byte
		if _, err := rand.Read(buf[:]); err != nil {
			panic(err)
		}
		connection.id = "snowflake-" + hex.EncodeToString(buf[:])
	}

	// Override with something that's not NullLogger to have real logging.
	connection.BytesLogger = &BytesNullLogger{}

	// Pipes remain the same even when DataChannel gets switched.
	connection.recvPipe, connection.writePipe = io.Pipe()

	err := connection.connect(config, broker)
	if err != nil {
		connection.Close()
		return nil, err
	}
	return connection, nil
}

// Read bytes from local SOCKS.
// As part of |io.ReadWriter|
func (c *WebRTCPeer) Read(b []byte) (int, error) {
	return c.recvPipe.Read(b)
}

// Writes bytes out to remote WebRTC.
// As part of |io.ReadWriter|
func (c *WebRTCPeer) Write(b []byte) (int, error) {
	c.lock.Lock()
	defer c.lock.Unlock()
	c.BytesLogger.AddOutbound(len(b))
	// TODO: Buffering could be improved / separated out of WebRTCPeer.
	if nil == c.transport {
		log.Printf("Buffered %d bytes --> WebRTC", len(b))
		c.buffer.Write(b)
	} else {
		c.transport.Send(b)
	}
	return len(b), nil
}

func (c *WebRTCPeer) Close() error {
	c.once.Do(func() {
		c.closed = true
		c.cleanup()
		log.Printf("WebRTC: Closing")
	})
	return nil
}

// Prevent long-lived broken remotes.
// Should also update the DataChannel in underlying go-webrtc's to make Closes
// more immediate / responsive.
func (c *WebRTCPeer) checkForStaleness() {
	c.lastReceive = time.Now()
	for {
		if c.closed {
			return
		}
		if time.Since(c.lastReceive) > SnowflakeTimeout {
			log.Printf("WebRTC: No messages received for %v -- closing stale connection.",
				SnowflakeTimeout)
			c.Close()
			return
		}
		<-time.After(time.Second)
	}
}

func (c *WebRTCPeer) connect(config *webrtc.Configuration, broker *BrokerChannel) error {
	log.Println(c.id, " connecting...")
	// TODO: When go-webrtc is more stable, it's possible that a new
	// PeerConnection won't need to be re-prepared each time.
	var err error
	c.pc, err = preparePeerConnection(config)
	if err != nil {
		return err
	}
	err = c.establishDataChannel()
	if err != nil {
		// nolint: golint
		return errors.New("WebRTC: Could not establish DataChannel")
	}
	answer := exchangeSDP(broker, c.pc.LocalDescription())
	log.Printf("Received Answer.\n")
	err = c.pc.SetRemoteDescription(*answer)
	if nil != err {
		log.Println("WebRTC: Unable to SetRemoteDescription:", err)
		return err
	}
	go c.checkForStaleness()
	return nil
}

// preparePeerConnection creates a new WebRTC PeerConnection and returns it
// after ICE candidate gathering is complete..
func preparePeerConnection(config *webrtc.Configuration) (*webrtc.PeerConnection, error) {
	pc, err := webrtc.NewPeerConnection(*config)
	if err != nil {
		log.Printf("NewPeerConnection ERROR: %s", err)
		return nil, err
	}
	// Prepare PeerConnection callbacks.
	offerChannel := make(chan struct{})
	// Allow candidates to accumulate until ICEGatheringStateComplete.
	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			log.Printf("WebRTC: Done gathering candidates")
			close(offerChannel)
		} else {
			log.Printf("WebRTC: Got ICE candidate: %s", candidate.String())
		}
	})

	offer, err := pc.CreateOffer(nil)
	// TODO: Potentially timeout and retry if ICE isn't working.
	if err != nil {
		log.Println("Failed to prepare offer", err)
		pc.Close()
		return nil, err
	}
	log.Println("WebRTC: Created offer")
	err = pc.SetLocalDescription(offer)
	if err != nil {
		log.Println("Failed to prepare offer", err)
		pc.Close()
		return nil, err
	}
	log.Println("WebRTC: Set local description")

	<-offerChannel // Wait for ICE candidate gathering to complete.
	log.Println("WebRTC: PeerConnection created.")
	return pc, nil
}

// Create a WebRTC DataChannel locally.
func (c *WebRTCPeer) establishDataChannel() error {
	c.lock.Lock()
	defer c.lock.Unlock()
	if c.transport != nil {
		panic("Unexpected datachannel already exists!")
	}
	ordered := true
	dataChannelOptions := &webrtc.DataChannelInit{
		Ordered: &ordered,
	}
	dc, err := c.pc.CreateDataChannel(c.id, dataChannelOptions)
	if err != nil {
		log.Printf("CreateDataChannel ERROR: %s", err)
		return err
	}
	dc.OnOpen(func() {
		c.lock.Lock()
		defer c.lock.Unlock()
		log.Println("WebRTC: DataChannel.OnOpen")
		if nil != c.transport {
			panic("WebRTC: transport already exists.")
		}
		// Flush buffered outgoing SOCKS data if necessary.
		if c.buffer.Len() > 0 {
			dc.Send(c.buffer.Bytes())
			log.Println("Flushed", c.buffer.Len(), "bytes.")
			c.buffer.Reset()
		}
		// Then enable the datachannel.
		c.transport = dc
	})
	dc.OnClose(func() {
		c.lock.Lock()
		// Future writes will go to the buffer until a new DataChannel is available.
		if nil == c.transport {
			// Closed locally, as part of a reset.
			log.Println("WebRTC: DataChannel.OnClose [locally]")
			c.lock.Unlock()
			return
		}
		// Closed remotely, need to reset everything.
		// Disable the DataChannel as a write destination.
		log.Println("WebRTC: DataChannel.OnClose [remotely]")
		c.transport = nil
		dc.Close()
		// Unlock before Close'ing, since it calls cleanup and asks for the
		// lock to check if the transport needs to be be deleted.
		c.lock.Unlock()
		c.Close()
	})
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		if len(msg.Data) <= 0 {
			log.Println("0 length message---")
		}
		n, err := c.writePipe.Write(msg.Data)
		c.BytesLogger.AddInbound(n)
		if err != nil {
			// TODO: Maybe shouldn't actually close.
			log.Println("Error writing to SOCKS pipe")
			if inerr := c.writePipe.CloseWithError(err); inerr != nil {
				log.Printf("c.writePipe.CloseWithError returned error: %v", inerr)
			}
		}
		c.lastReceive = time.Now()
	})
	log.Println("WebRTC: DataChannel created.")
	return nil
}

// exchangeSDP sends the local SDP offer to the Broker, awaits the SDP answer,
// and returns the answer.
func exchangeSDP(broker *BrokerChannel, offer *webrtc.SessionDescription) *webrtc.SessionDescription {
	// Keep trying the same offer until a valid answer arrives.
	for {
		// Send offer to broker (blocks).
		answer, err := broker.Negotiate(offer)
		if err == nil {
			return answer
		}
		log.Printf("BrokerChannel Error: %s", err)
		log.Printf("Failed to retrieve answer. Retrying in %v", ReconnectTimeout)
		<-time.After(ReconnectTimeout)
	}
}

// Close all channels and transports
func (c *WebRTCPeer) cleanup() {
	// Close this side of the SOCKS pipe.
	if nil != c.writePipe {
		c.writePipe.Close()
		c.writePipe = nil
	}
	c.lock.Lock()
	if nil != c.transport {
		log.Printf("WebRTC: closing DataChannel")
		dataChannel := c.transport
		// Setting transport to nil *before* dc Close indicates to OnClose that
		// this was locally triggered.
		c.transport = nil
		// Release the lock before calling DeleteDataChannel (which in turn
		// calls Close on the dataChannel), but after nil'ing out the transport,
		// since otherwise we'll end up in the onClose handler in a deadlock.
		c.lock.Unlock()
		if c.pc == nil {
			panic("DataChannel w/o PeerConnection, not good.")
		}
		dataChannel.Close()
	} else {
		c.lock.Unlock()
	}
	if nil != c.pc {
		log.Printf("WebRTC: closing PeerConnection")
		err := c.pc.Close()
		if nil != err {
			log.Printf("Error closing peerconnection...")
		}
		c.pc = nil
	}
}
