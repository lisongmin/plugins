package dhcp4client

import (
	"bytes"
	"fmt"
	"hash/fnv"
	"math/rand"
	"net"
	"sync"
	"syscall"
	"time"

	"github.com/d2g/dhcp4"
)

const (
	MaxDHCPLen = 576
)

type Client struct {
	hardwareAddr  net.HardwareAddr //The HardwareAddr to send in the request.
	ignoreServers []net.IP         //List of Servers to Ignore requests from.
	timeout       time.Duration    //Time before we timeout.
	broadcast     bool             //Set the Bcast flag in BOOTP Flags
	connection    ConnectionInt    //The Connection Method to use
	generateXID   func([]byte)     //Function Used to Generate a XID
}

//Abstracts the type of underlying socket used
type ConnectionInt interface {
	Close() error
	Write(packet []byte) error
	ReadFrom() ([]byte, net.IP, error)
	SetReadTimeout(t time.Duration) error
}

func New(options ...func(*Client) error) (*Client, error) {
	c := Client{
		timeout:   time.Second * 10,
		broadcast: true,
	}

	err := c.SetOption(options...)
	if err != nil {
		return nil, err
	}

	if c.generateXID == nil {
		// https://tools.ietf.org/html/rfc2131#section-4.1 explains:
		//
		// A DHCP client MUST choose 'xid's in such a way as to minimize the chance
		// of using an 'xid' identical to one used by another client.
		//
		// Hence, seed a random number generator with the current time and hardware
		// address.
		h := fnv.New64()
		h.Write(c.hardwareAddr)
		seed := int64(h.Sum64()) + time.Now().Unix()
		rnd := rand.New(rand.NewSource(seed))
		var rndMu sync.Mutex
		c.generateXID = func(b []byte) {
			rndMu.Lock()
			defer rndMu.Unlock()
			rnd.Read(b)
		}
	}

	//if connection hasn't been set as an option create the default.
	if c.connection == nil {
		conn, err := NewInetSock()
		if err != nil {
			return nil, err
		}
		c.connection = conn
	}

	return &c, nil
}

func (c *Client) SetOption(options ...func(*Client) error) error {
	for _, opt := range options {
		if err := opt(c); err != nil {
			return err
		}
	}
	return nil
}

func Timeout(t time.Duration) func(*Client) error {
	return func(c *Client) error {
		c.timeout = t
		return nil
	}
}

func IgnoreServers(s []net.IP) func(*Client) error {
	return func(c *Client) error {
		c.ignoreServers = s
		return nil
	}
}

func HardwareAddr(h net.HardwareAddr) func(*Client) error {
	return func(c *Client) error {
		c.hardwareAddr = h
		return nil
	}
}

func Broadcast(b bool) func(*Client) error {
	return func(c *Client) error {
		c.broadcast = b
		return nil
	}
}

func Connection(conn ConnectionInt) func(*Client) error {
	return func(c *Client) error {
		c.connection = conn
		return nil
	}
}

func GenerateXID(g func([]byte)) func(*Client) error {
	return func(c *Client) error {
		c.generateXID = g
		return nil
	}
}

//Close Connections
func (c *Client) Close() error {
	if c.connection != nil {
		return c.connection.Close()
	}
	return nil
}

//Send the Discovery Packet to the Broadcast Channel
func (c *Client) SendDiscoverPacket() (dhcp4.Packet, error) {
	discoveryPacket := c.DiscoverPacket()
	discoveryPacket.PadToMinSize()

	return discoveryPacket, c.SendPacket(discoveryPacket)
}

// TimeoutError records a timeout when waiting for a DHCP packet.
type TimeoutError struct {
	Timeout time.Duration
}

func (te *TimeoutError) Error() string {
	return fmt.Sprintf("no DHCP packet received within %v", te.Timeout)
}

//Retreive Offer...
//Wait for the offer for a specific Discovery Packet.
func (c *Client) GetOffer(discoverPacket *dhcp4.Packet) (dhcp4.Packet, error) {
	start := time.Now()

	for {
		timeout := c.timeout - time.Since(start)
		if timeout < 0 {
			return dhcp4.Packet{}, &TimeoutError{Timeout: c.timeout}
		}

		c.connection.SetReadTimeout(timeout)
		readBuffer, source, err := c.connection.ReadFrom()
		if err != nil {
			if errno, ok := err.(syscall.Errno); ok && errno == syscall.EAGAIN {
				return dhcp4.Packet{}, &TimeoutError{Timeout: c.timeout}
			}
			return dhcp4.Packet{}, err
		}

		offerPacket := dhcp4.Packet(readBuffer)
		offerPacketOptions := offerPacket.ParseOptions()

		// Ignore Servers in my Ignore list
		for _, ignoreServer := range c.ignoreServers {
			if source.Equal(ignoreServer) {
				continue
			}

			if offerPacket.SIAddr().Equal(ignoreServer) {
				continue
			}
		}

		if len(offerPacketOptions[dhcp4.OptionDHCPMessageType]) < 1 || dhcp4.MessageType(offerPacketOptions[dhcp4.OptionDHCPMessageType][0]) != dhcp4.Offer || !bytes.Equal(discoverPacket.XId(), offerPacket.XId()) {
			continue
		}

		return offerPacket, nil
	}

}

//Send Request Based On the offer Received.
func (c *Client) SendRequest(offerPacket *dhcp4.Packet) (dhcp4.Packet, error) {
	requestPacket := c.RequestPacket(offerPacket)
	requestPacket.PadToMinSize()

	return requestPacket, c.SendPacket(requestPacket)
}

//Retreive Acknowledgement
//Wait for the offer for a specific Request Packet.
func (c *Client) GetAcknowledgement(requestPacket *dhcp4.Packet) (dhcp4.Packet, error) {
	start := time.Now()

	for {
		timeout := c.timeout - time.Since(start)
		if timeout < 0 {
			return dhcp4.Packet{}, &TimeoutError{Timeout: c.timeout}
		}

		c.connection.SetReadTimeout(timeout)
		readBuffer, source, err := c.connection.ReadFrom()
		if err != nil {
			if errno, ok := err.(syscall.Errno); ok && errno == syscall.EAGAIN {
				return dhcp4.Packet{}, &TimeoutError{Timeout: c.timeout}
			}
			return dhcp4.Packet{}, err
		}

		acknowledgementPacket := dhcp4.Packet(readBuffer)
		acknowledgementPacketOptions := acknowledgementPacket.ParseOptions()

		// Ignore Servers in my Ignore list
		for _, ignoreServer := range c.ignoreServers {
			if source.Equal(ignoreServer) {
				continue
			}

			if acknowledgementPacket.SIAddr().Equal(ignoreServer) {
				continue
			}
		}

		if !bytes.Equal(requestPacket.XId(), acknowledgementPacket.XId()) || len(acknowledgementPacketOptions[dhcp4.OptionDHCPMessageType]) < 1 || (dhcp4.MessageType(acknowledgementPacketOptions[dhcp4.OptionDHCPMessageType][0]) != dhcp4.ACK && dhcp4.MessageType(acknowledgementPacketOptions[dhcp4.OptionDHCPMessageType][0]) != dhcp4.NAK) {
			continue
		}

		return acknowledgementPacket, nil
	}
}

//Send Decline to the received acknowledgement.
func (c *Client) SendDecline(acknowledgementPacket *dhcp4.Packet) (dhcp4.Packet, error) {
	declinePacket := c.DeclinePacket(acknowledgementPacket)
	declinePacket.PadToMinSize()

	return declinePacket, c.SendPacket(declinePacket)
}

//Send a DHCP Packet.
func (c *Client) SendPacket(packet dhcp4.Packet) error {
	return c.connection.Write(packet)
}

//Create Discover Packet
func (c *Client) DiscoverPacket() dhcp4.Packet {
	messageid := make([]byte, 4)
	c.generateXID(messageid)

	packet := dhcp4.NewPacket(dhcp4.BootRequest)
	packet.SetCHAddr(c.hardwareAddr)
	packet.SetXId(messageid)
	packet.SetBroadcast(c.broadcast)

	packet.AddOption(dhcp4.OptionDHCPMessageType, []byte{byte(dhcp4.Discover)})
	//packet.PadToMinSize()
	return packet
}

//Create Request Packet
func (c *Client) RequestPacket(offerPacket *dhcp4.Packet) dhcp4.Packet {
	offerOptions := offerPacket.ParseOptions()

	packet := dhcp4.NewPacket(dhcp4.BootRequest)
	packet.SetCHAddr(c.hardwareAddr)

	packet.SetXId(offerPacket.XId())
	packet.SetCIAddr(offerPacket.CIAddr())
	packet.SetSIAddr(offerPacket.SIAddr())

	packet.SetBroadcast(c.broadcast)
	packet.AddOption(dhcp4.OptionDHCPMessageType, []byte{byte(dhcp4.Request)})
	packet.AddOption(dhcp4.OptionRequestedIPAddress, (offerPacket.YIAddr()).To4())
	packet.AddOption(dhcp4.OptionServerIdentifier, offerOptions[dhcp4.OptionServerIdentifier])

	return packet
}

//Create Request Packet For a Renew
func (c *Client) RenewalRequestPacket(acknowledgement *dhcp4.Packet) dhcp4.Packet {
	messageid := make([]byte, 4)
	c.generateXID(messageid)

	packet := dhcp4.NewPacket(dhcp4.BootRequest)
	packet.SetCHAddr(acknowledgement.CHAddr())

	packet.SetXId(messageid)
	packet.SetCIAddr(acknowledgement.YIAddr())
	packet.SetSIAddr(acknowledgement.SIAddr())

	// Following options must not be sent in renew
	// - Requested IP Address
	// - Server Identifier
	//
	// and renew must using unicast but not broadcast.
	//
	// See https://datatracker.ietf.org/doc/html/rfc2131#section-4.3.6 for details
	packet.AddOption(dhcp4.OptionDHCPMessageType, []byte{byte(dhcp4.Request)})

	return packet
}

//Create Release Packet For a Release
func (c *Client) ReleasePacket(acknowledgement *dhcp4.Packet) dhcp4.Packet {
	messageid := make([]byte, 4)
	c.generateXID(messageid)

	acknowledgementOptions := acknowledgement.ParseOptions()

	packet := dhcp4.NewPacket(dhcp4.BootRequest)
	packet.SetCHAddr(acknowledgement.CHAddr())

	packet.SetXId(messageid)
	packet.SetCIAddr(acknowledgement.YIAddr())

	packet.AddOption(dhcp4.OptionDHCPMessageType, []byte{byte(dhcp4.Release)})
	packet.AddOption(dhcp4.OptionServerIdentifier, acknowledgementOptions[dhcp4.OptionServerIdentifier])

	return packet
}

//Create Decline Packet
func (c *Client) DeclinePacket(acknowledgement *dhcp4.Packet) dhcp4.Packet {
	messageid := make([]byte, 4)
	c.generateXID(messageid)

	acknowledgementOptions := acknowledgement.ParseOptions()

	packet := dhcp4.NewPacket(dhcp4.BootRequest)
	packet.SetCHAddr(acknowledgement.CHAddr())
	packet.SetXId(messageid)

	packet.AddOption(dhcp4.OptionDHCPMessageType, []byte{byte(dhcp4.Decline)})
	packet.AddOption(dhcp4.OptionRequestedIPAddress, (acknowledgement.YIAddr()).To4())
	packet.AddOption(dhcp4.OptionServerIdentifier, acknowledgementOptions[dhcp4.OptionServerIdentifier])

	return packet
}

//Lets do a Full DHCP Request.
func (c *Client) Request() (bool, dhcp4.Packet, error) {
	discoveryPacket, err := c.SendDiscoverPacket()
	if err != nil {
		return false, discoveryPacket, err
	}

	offerPacket, err := c.GetOffer(&discoveryPacket)
	if err != nil {
		return false, offerPacket, err
	}

	requestPacket, err := c.SendRequest(&offerPacket)
	if err != nil {
		return false, requestPacket, err
	}

	acknowledgement, err := c.GetAcknowledgement(&requestPacket)
	if err != nil {
		return false, acknowledgement, err
	}

	acknowledgementOptions := acknowledgement.ParseOptions()
	if dhcp4.MessageType(acknowledgementOptions[dhcp4.OptionDHCPMessageType][0]) != dhcp4.ACK {
		return false, acknowledgement, nil
	}

	return true, acknowledgement, nil
}

//Renew a lease backed on the Acknowledgement Packet.
//Returns Sucessfull, The AcknoledgementPacket, Any Errors
func (c *Client) Renew(acknowledgement dhcp4.Packet) (bool, dhcp4.Packet, error) {
	renewRequest := c.RenewalRequestPacket(&acknowledgement)
	renewRequest.PadToMinSize()

	err := c.SendPacket(renewRequest)
	if err != nil {
		return false, renewRequest, err
	}

	newAcknowledgement, err := c.GetAcknowledgement(&renewRequest)
	if err != nil {
		return false, newAcknowledgement, err
	}

	newAcknowledgementOptions := newAcknowledgement.ParseOptions()
	if dhcp4.MessageType(newAcknowledgementOptions[dhcp4.OptionDHCPMessageType][0]) != dhcp4.ACK {
		return false, newAcknowledgement, nil
	}

	return true, newAcknowledgement, nil
}

//Release a lease backed on the Acknowledgement Packet.
//Returns Any Errors
func (c *Client) Release(acknowledgement dhcp4.Packet) error {
	release := c.ReleasePacket(&acknowledgement)
	release.PadToMinSize()

	return c.SendPacket(release)
}
