package pcap

import (
	"fmt"
	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/xtaci/kcp-go"
	"ikago/internal/addr"
	"ikago/internal/config"
	"ikago/internal/crypto"
	"ikago/internal/log"
	"net"
	"sync"
	"time"
)

type clientIndicator struct {
	crypt crypto.Crypt
	seq   uint32
	ack   uint32
}

const establishDeadline time.Duration = 3 * time.Second
const keepFragments time.Duration = 30 * time.Second

// Conn is a packet pcap network connection add fake TCP header to all traffic.
type Conn struct {
	lock          sync.Mutex
	conn          *RawConn
	defrag        Defragmenter
	srcPort       uint16
	dstAddr       *net.TCPAddr
	crypt         crypto.Crypt
	mtu           int
	appear        time.Time
	isConnected   bool
	isReconnected bool
	isClosed      bool
	clientsLock   sync.RWMutex
	clients       map[string]*clientIndicator
	id            uint16
	readDeadline  time.Time
	writeDeadline time.Time
}

func newConn() *Conn {
	conn := &Conn{
		defrag:  NewEasyDefragmenter(),
		mtu:     MaxMTU,
		clients: make(map[string]*clientIndicator),
	}
	conn.defrag.SetDeadline(keepFragments)
	return conn
}

// Dial acts like Dial for pcap networks.
func Dial(srcDev, dstDev *Device, srcPort uint16, dstAddr *net.TCPAddr, crypt crypto.Crypt, mtu int, timeout int) (*Conn, error) {
	srcAddr := &net.TCPAddr{
		IP:   srcDev.IPAddr().IP,
		Port: int(srcPort),
	}

	conn, err := dialPassive(srcDev, dstDev, srcPort, dstAddr, crypt, mtu)
	if err != nil {
		return nil, &net.OpError{
			Op:     "dial",
			Net:    "pcap",
			Source: srcAddr,
			Addr:   dstAddr,
			Err:    err,
		}
	}

	log.Infof("Connect to server %s\n", dstAddr.String())

	conn.appear = time.Now()

	// Handshake
	err = conn.handshakeSYN()
	if err != nil {
		return nil, &net.OpError{
			Op:     "dial",
			Net:    "pcap",
			Source: srcAddr,
			Addr:   dstAddr,
			Err:    fmt.Errorf("handshake: %w", err),
		}
	}

	go func() {
		time.Sleep(establishDeadline)

		if !conn.isConnected {
			log.Errorf("Cannot receive response from server %s, is it down?\n", dstAddr.String())
		}
	}()

	// Timeout
	if timeout > 0 {
		go func() {
			for {
				time.Sleep(time.Duration(timeout) * time.Second)

				if !conn.isClosed {
					conn.isReconnected = false

					err = conn.handshakeSYN()
					if err != nil {
						log.Errorf("%w", &net.OpError{
							Op:     "dial",
							Net:    "pcap",
							Source: srcAddr,
							Addr:   dstAddr,
							Err:    fmt.Errorf("handshake: %w", err),
						})
					}

					go func() {
						time.Sleep(establishDeadline)

						if !conn.isReconnected {
							log.Errorf("Cannot receive response from server %s, is it down?\n", dstAddr.String())
						}
					}()
				}
			}
		}()
	}

	return conn, nil
}

func dialPassive(srcDev, dstDev *Device, srcPort uint16, dstAddr *net.TCPAddr, crypt crypto.Crypt, mtu int) (*Conn, error) {
	srcAddr := &net.TCPAddr{
		IP:   srcDev.IPAddr().IP,
		Port: int(srcPort),
	}

	filter, err := addr.SrcBPFFilter(dstAddr)
	if err != nil {
		return nil, fmt.Errorf("parse filter %s: %w", dstAddr, err)
	}
	dstIP := &net.IPAddr{IP: dstAddr.IP}
	filter2, err := addr.SrcBPFFilter(dstIP)
	if err != nil {
		return nil, fmt.Errorf("parse filter %s: %w", dstIP, err)
	}

	rawConn, err := CreateRawConn(srcDev, dstDev, fmt.Sprintf("ip && ((tcp && dst port %d && %s) || ((ip[6:2] & 0x1fff) != 0 && %s))", srcAddr.Port, filter, filter2))
	if err != nil {
		return nil, fmt.Errorf("create raw connection: %w", err)
	}

	conn := newConn()
	conn.srcPort = srcPort
	conn.dstAddr = dstAddr
	conn.crypt = crypt
	conn.mtu = mtu
	conn.conn = rawConn

	return conn, nil
}

func listenMulticast(srcDev, dstDev *Device, srcPort uint16, crypt crypto.Crypt, mtu int) (*Conn, error) {
	addrs := make([]*net.TCPAddr, 0)
	for _, ip := range srcDev.IPAddrs() {
		addrs = append(addrs, &net.TCPAddr{IP: ip.IP, Port: int(srcPort)})
	}
	srcAddrs := addr.MultiTCPAddr{Addrs: addrs}

	rawConn, err := CreateRawConn(srcDev, dstDev, fmt.Sprintf("tcp && dst port %d", srcPort))
	if err != nil {
		return nil, &net.OpError{
			Op:     "dial",
			Net:    "pcap",
			Source: srcAddrs,
			Err:    fmt.Errorf("create connection: %w", err),
		}
	}

	conn := newConn()
	conn.srcPort = srcPort
	conn.crypt = crypt
	conn.mtu = mtu
	conn.conn = rawConn

	return conn, nil
}

func (c *Conn) Read(b []byte) (n int, err error) {
	n, _, err = c.ReadFrom(b)

	return n, err
}

func (c *Conn) handshakeSYN() error {
	var (
		transportLayer gopacket.SerializableLayer
		networkLayer   gopacket.SerializableLayer
		linkLayer      gopacket.SerializableLayer
	)

	c.lock.Lock()
	defer c.lock.Unlock()

	// Client
	c.clientsLock.RLock()
	client, ok := c.clients[c.RemoteAddr().String()]
	c.clientsLock.RUnlock()
	if !ok {
		// Initial TCP Seq
		client = &clientIndicator{
			crypt: c.crypt,
			seq:   0,
		}

		// Map client
		c.clientsLock.Lock()
		c.clients[c.RemoteAddr().String()] = client
		c.clientsLock.Unlock()
	}

	// Create layers
	transportLayer, networkLayer, linkLayer, err := CreateLayers(c.srcPort, uint16(c.dstAddr.Port), client.seq, client.ack, c.conn, c.dstAddr.IP, c.id, 128, c.RemoteDev().HardwareAddr())
	if err != nil {
		return err
	}

	// Make TCP layer SYN
	FlagTCPLayer(transportLayer.(*layers.TCP), true, false, false)

	// Serialize layers
	data, err := Serialize(linkLayer, networkLayer, transportLayer)
	if err != nil {
		return fmt.Errorf("serialize: %w", err)
	}

	// Write packet data
	_, err = c.conn.Write(data)
	if err != nil {
		return fmt.Errorf("write: %w", err)
	}

	// TCP Seq
	client.seq++

	// IPv4 Id
	if networkLayer.LayerType() == layers.LayerTypeIPv4 {
		c.id++
	}

	srcAddr := &net.TCPAddr{
		IP:   c.LocalDev().IPAddr().IP,
		Port: int(c.srcPort),
	}
	log.Verbosef("Send TCP SYN: %s -> %s\n", srcAddr.String(), c.RemoteAddr().String())

	return nil
}

func (c *Conn) handshakeSYNACK(indicator *PacketIndicator) error {
	var (
		err               error
		newTransportLayer gopacket.SerializableLayer
		newNetworkLayer   gopacket.SerializableLayer
		newLinkLayer      gopacket.SerializableLayer
	)

	c.lock.Lock()
	defer c.lock.Unlock()

	// Client
	c.clientsLock.RLock()
	client, ok := c.clients[indicator.Src().String()]
	c.clientsLock.RUnlock()
	if !ok {
		// Initial TCP Seq
		client = &clientIndicator{
			crypt: c.crypt,
			seq:   0,
		}

		// Map client
		c.clientsLock.Lock()
		c.clients[indicator.Src().String()] = client
		c.clientsLock.Unlock()
	}
	client.ack = indicator.TCPLayer().Seq + 1

	// Create layers
	newTransportLayer, newNetworkLayer, newLinkLayer, err = CreateLayers(indicator.DstPort(), indicator.SrcPort(), client.seq, client.ack, c.conn, indicator.SrcIP(), c.id, 64, indicator.SrcHardwareAddr())
	if err != nil {
		return fmt.Errorf("create layers: %w", err)
	}

	// Make TCP layer SYN & ACK
	FlagTCPLayer(newTransportLayer.(*layers.TCP), true, false, true)

	// Serialize layers
	data, err := Serialize(newLinkLayer, newNetworkLayer, newTransportLayer)
	if err != nil {
		return fmt.Errorf("serialize: %w", err)
	}

	// Write packet data
	_, err = c.conn.Write(data)
	if err != nil {
		return fmt.Errorf("write: %w", err)
	}

	// TCP Seq
	client.seq++

	// IPv4 Id
	if newNetworkLayer.LayerType() == layers.LayerTypeIPv4 {
		c.id++
	}

	srcAddr := &net.TCPAddr{
		IP:   c.LocalDev().IPAddr().IP,
		Port: int(indicator.DstPort()),
	}
	log.Verbosef("Send TCP SYN+ACK: %s <- %s\n", indicator.Src().String(), srcAddr.String())

	return nil
}

func (c *Conn) handshakeACK(indicator *PacketIndicator) error {
	var (
		err               error
		newTransportLayer gopacket.SerializableLayer
		newNetworkLayer   gopacket.SerializableLayer
		newLinkLayer      gopacket.SerializableLayer
	)

	c.lock.Lock()
	defer c.lock.Unlock()

	// Client
	c.clientsLock.RLock()
	client, ok := c.clients[indicator.Src().String()]
	c.clientsLock.RUnlock()
	if !ok {
		return fmt.Errorf("client %s unauthorized", indicator.Src().String())
	}

	// TCP Ack
	client.ack = indicator.TCPLayer().Seq + 1

	// Create layers
	newTransportLayer, newNetworkLayer, newLinkLayer, err = CreateLayers(indicator.DstPort(), indicator.SrcPort(), client.seq, client.ack, c.conn, indicator.SrcIP(), c.id, 128, indicator.SrcHardwareAddr())
	if err != nil {
		return fmt.Errorf("create layers: %w", err)
	}

	// Make TCP layer ACK
	FlagTCPLayer(newTransportLayer.(*layers.TCP), false, false, true)

	// Serialize layers
	data, err := Serialize(newLinkLayer, newNetworkLayer, newTransportLayer)
	if err != nil {
		return fmt.Errorf("serialize: %w", err)
	}

	// Write packet data
	_, err = c.conn.Write(data)
	if err != nil {
		return fmt.Errorf("write: %w", err)
	}

	// IPv4 Id
	if newNetworkLayer.LayerType() == layers.LayerTypeIPv4 {
		c.id++
	}

	srcAddr := &net.TCPAddr{
		IP:   c.LocalDev().IPAddr().IP,
		Port: int(indicator.DstPort()),
	}
	log.Verbosef("Send TCP ACK: %s -> %s\n", srcAddr.String(), indicator.Src().String())

	return nil
}

func (c *Conn) Write(b []byte) (n int, err error) {
	return c.WriteTo(b, c.RemoteAddr())
}

func (c *Conn) ReadFrom(p []byte) (n int, a net.Addr, err error) {
	packet, a, err := c.readPacketFrom()
	if err != nil {
		return 0, a, &net.OpError{
			Op:     "read",
			Net:    "pcap",
			Source: c.LocalAddr(),
			Addr:   a,
			Err:    err,
		}
	}

	// Reply SYN
	if packet.TransportLayer().(*layers.TCP).SYN {
		indicator, err := ParsePacket(packet)
		if err != nil {
			return 0, a, &net.OpError{
				Op:     "read",
				Net:    "pcap",
				Source: c.LocalAddr(),
				Addr:   a,
				Err:    fmt.Errorf("parse packet: %w", err),
			}
		}

		// SYN+ACK
		if indicator.TCPLayer().ACK {
			log.Verbosef("Receive TCP SYN+ACK: %s <- %s\n", indicator.Dst().String(), a.String())

			if !c.isConnected {
				t := time.Now()
				duration := t.Sub(c.appear)

				log.Infof("Connected to server %s in %.3f ms (RTT)\n", a.String(), float64(duration.Microseconds())/1000)

				c.isConnected = true
			}
			c.isReconnected = true

			err = c.handshakeACK(indicator)
		} else {
			log.Verbosef("Receive TCP SYN: %s -> %s\n", a.String(), indicator.Dst().String())

			err = c.handshakeSYNACK(indicator)
		}
		if err != nil {
			return 0, a, &net.OpError{
				Op:     "read",
				Net:    "pcap",
				Source: c.LocalAddr(),
				Addr:   a,
				Err:    fmt.Errorf("handshake: %w", err),
			}
		}

		return 0, a, nil
	}

	if packet.ApplicationLayer() == nil {
		return 0, a, nil
	}

	// Client
	c.clientsLock.RLock()
	client, ok := c.clients[a.String()]
	c.clientsLock.RUnlock()
	if !ok {
		return 0, a, &net.OpError{
			Op:     "read",
			Net:    "pcap",
			Source: c.LocalAddr(),
			Addr:   a,
			Err:    fmt.Errorf("client %s unauthorized", a.String()),
		}
	}

	// TCP Ack, always use the expected one
	expectedAck := packet.TransportLayer().(*layers.TCP).Seq + uint32(len(packet.ApplicationLayer().LayerContents()))
	if expectedAck > client.ack || (4294967295-packet.TransportLayer().(*layers.TCP).Seq < uint32(len(packet.ApplicationLayer().LayerContents()))) {
		client.ack = expectedAck
	}

	// Decrypt
	contents, err := client.crypt.Decrypt(packet.ApplicationLayer().LayerContents())
	if err != nil {
		return 0, a, &net.OpError{
			Op:     "read",
			Net:    "pcap",
			Source: c.LocalAddr(),
			Addr:   a,
			Err:    fmt.Errorf("decrypt: %w", err),
		}
	}

	copy(p, contents)

	return len(contents), a, err
}

func (c *Conn) readPacketFrom() (gopacket.Packet, net.Addr, error) {
	type tuple struct {
		packet gopacket.Packet
		err    error
	}

	ch := make(chan tuple)
	go func() {
		for {
			packet, err := c.conn.ReadPacket()
			if err != nil {
				ch <- tuple{err: err}
				return
			}

			// Parse packet
			indicator, err := ParsePacket(packet)
			if err != nil {
				ch <- tuple{err: fmt.Errorf("parse packet: %w", err)}
				return
			}

			// Handle fragments
			indicator, err = c.defrag.Append(indicator)
			if err != nil {
				ch <- tuple{err: fmt.Errorf("defrag: %w", err)}
				return
			}
			if indicator != nil {
				ch <- tuple{packet: indicator.packet}
				return
			}
		}
	}()
	// Timeout
	if !c.readDeadline.IsZero() {
		go func() {
			duration := c.readDeadline.Sub(time.Now())
			if duration > 0 {
				time.Sleep(duration)
			}
			ch <- tuple{err: &timeoutError{Err: "timeout"}}
		}()
	}

	tu := <-ch
	if tu.err != nil {
		return nil, nil, tu.err
	}

	// Parse packet
	indicator, err := ParsePacket(tu.packet)
	if err != nil {
		return nil, nil, fmt.Errorf("parse packet: %w", err)
	}

	switch t := indicator.TransportLayer().LayerType(); t {
	case layers.LayerTypeTCP:
		return tu.packet, &net.UDPAddr{
			IP:   indicator.SrcIP(),
			Port: int(indicator.SrcPort()),
		}, nil
	case layers.LayerTypeUDP:
		return tu.packet, indicator.Src(), nil
	default:
		return nil, indicator.Src(), fmt.Errorf("transport layer type %s not support", t)
	}
}

func (c *Conn) WriteTo(p []byte, addr net.Addr) (n int, err error) {
	var (
		dstIP   net.IP
		dstPort uint16
	)

	ch := make(chan error)

	switch t := addr.(type) {
	case *net.TCPAddr:
		dstIP = addr.(*net.TCPAddr).IP
		dstPort = uint16(addr.(*net.TCPAddr).Port)
	case *net.UDPAddr:
		dstIP = addr.(*net.UDPAddr).IP
		dstPort = uint16(addr.(*net.UDPAddr).Port)
	default:
		return 0, &net.OpError{
			Op:     "write",
			Net:    "pcap",
			Source: c.LocalAddr(),
			Addr:   addr,
			Err:    fmt.Errorf("type %T not support", t),
		}
	}

	go func() {
		var (
			transportLayer gopacket.SerializableLayer
			networkLayer   gopacket.SerializableLayer
			linkLayer      gopacket.SerializableLayer
			fragments      [][]byte
		)

		c.lock.Lock()
		defer c.lock.Unlock()

		// Client
		c.clientsLock.RLock()
		client, ok := c.clients[addr.String()]
		c.clientsLock.RUnlock()
		if !ok {
			ch <- fmt.Errorf("client %s unrecognized", addr.String())
			return
		}

		// Create layers
		transportLayer, networkLayer, linkLayer, err := CreateLayers(c.srcPort, dstPort, client.seq, client.ack, c.conn, dstIP, c.id, 128, c.conn.RemoteDev().HardwareAddr())
		if err != nil {
			ch <- fmt.Errorf("create layers: %w", err)
			return
		}

		// Encrypt
		contents, err := client.crypt.Encrypt(p)
		if err != nil {
			ch <- fmt.Errorf("encrypt: %w", err)
			return
		}

		// Fragment
		fragments, err = CreateFragmentPackets(linkLayer.(gopacket.Layer), networkLayer.(gopacket.Layer), transportLayer.(gopacket.Layer), gopacket.Payload(contents), c.mtu)
		if err != nil {
			ch <- fmt.Errorf("fragment: %w", err)
			return
		}

		// Write packet data
		for _, frag := range fragments {
			_, err := c.conn.Write(frag)
			if err != nil {
				ch <- fmt.Errorf("write: %w", err)
				return
			}
		}

		// TCP Seq
		client.seq = client.seq + uint32(len(contents))

		// IPv4 Id
		if networkLayer.LayerType() == layers.LayerTypeIPv4 {
			c.id++
		}

		ch <- nil
		return
	}()
	// Timeout
	if !c.writeDeadline.IsZero() {
		go func() {
			duration := c.readDeadline.Sub(time.Now())
			if duration > 0 {
				time.Sleep(duration)
			}
			ch <- &timeoutError{Err: "timeout"}
		}()
	}

	err = <-ch
	if err != nil {
		return 0, &net.OpError{
			Op:     "write",
			Net:    "pcap",
			Source: c.LocalAddr(),
			Addr:   addr,
			Err:    err,
		}
	}

	return len(p), nil
}

func (c *Conn) Close() error {
	c.isClosed = true

	err := c.conn.Close()
	if err != nil {
		return &net.OpError{
			Op:   "close",
			Net:  "pcap",
			Addr: c.LocalAddr(),
			Err:  err,
		}
	}

	return nil
}

// LocalDev returns the local device.
func (c *Conn) LocalDev() *Device {
	return c.conn.LocalDev()
}

func (c *Conn) LocalAddr() net.Addr {
	return &net.UDPAddr{IP: c.LocalDev().IPAddr().IP, Port: int(c.srcPort)}
}

// RemoteDev returns the remote device.
func (c *Conn) RemoteDev() *Device {
	return c.conn.RemoteDev()
}

func (c *Conn) RemoteAddr() net.Addr {
	return c.dstAddr
}

func (c *Conn) SetDeadline(t time.Time) error {
	readDeadline := c.readDeadline

	err := c.SetReadDeadline(t)
	if err != nil {
		return err
	}

	err = c.SetWriteDeadline(t)
	if err != nil {
		_ = c.SetReadDeadline(readDeadline)
		return err
	}

	return nil
}

func (c *Conn) SetReadDeadline(t time.Time) error {
	c.readDeadline = t

	return nil
}

func (c *Conn) SetWriteDeadline(t time.Time) error {
	c.writeDeadline = t

	return nil
}

// Listener is a pcap network listener.
type Listener struct {
	conn    *RawConn
	srcPort uint16
	crypt   crypto.Crypt
	mtu     int
	clients map[string]net.Conn
}

// Listen acts like Listen for pcap networks.
func Listen(srcDev, dstDev *Device, srcPort uint16, crypt crypto.Crypt, mtu int) (*Listener, error) {
	addrs := make([]*net.TCPAddr, 0)
	for _, ip := range srcDev.IPAddrs() {
		addrs = append(addrs, &net.TCPAddr{IP: ip.IP, Port: int(srcPort)})
	}
	srcAddrs := addr.MultiTCPAddr{Addrs: addrs}

	conn, err := CreateRawConn(srcDev, dstDev, fmt.Sprintf("tcp && tcp[tcpflags] & tcp-syn != 0 && dst port %d", srcPort))
	if err != nil {
		return nil, &net.OpError{
			Op:     "dial",
			Net:    "pcap",
			Source: srcAddrs,
			Err:    fmt.Errorf("create handshake connection: %w", err),
		}
	}

	listener := &Listener{
		conn:    conn,
		srcPort: srcPort,
		crypt:   crypt,
		mtu:     mtu,
		clients: make(map[string]net.Conn),
	}

	return listener, nil
}

func (l *Listener) Accept() (net.Conn, error) {
	packet, err := l.conn.ReadPacket()
	if err != nil {
		return nil, &net.OpError{
			Op:   "accept",
			Net:  "pcap",
			Addr: l.Addr(),
			Err:  fmt.Errorf("read device %s: %w", l.Dev().Alias(), err),
		}
	}

	// Parse packet
	indicator, err := ParsePacket(packet)
	if err != nil {
		return nil, &net.OpError{
			Op:   "accept",
			Net:  "pcap",
			Addr: l.Addr(),
			Err:  fmt.Errorf("parse packet: %w", err),
		}
	}

	_, ok := l.clients[indicator.Src().String()]
	if ok {
		// Duplicate
		return nil, nil
	}

	conn, err := dialPassive(l.Dev(), l.conn.RemoteDev(), l.srcPort, indicator.Src().(*net.TCPAddr), l.crypt, l.mtu)
	if err != nil {
		return nil, &net.OpError{
			Op:     "dial",
			Net:    "pcap",
			Source: l.Addr(),
			Addr:   indicator.Src(),
			Err:    err,
		}
	}

	conn.clients[indicator.Src().String()] = &clientIndicator{
		crypt: l.crypt,
		seq:   0,
		ack:   0,
	}

	// Handshaking with client (SYN+ACK)
	err = conn.handshakeSYNACK(indicator)
	if err != nil {
		return nil, &net.OpError{
			Op:     "handshake",
			Net:    "pcap",
			Source: l.Addr(),
			Addr:   indicator.Src(),
			Err:    err,
		}
	}

	// Map client
	l.clients[indicator.Src().String()] = conn

	return conn, nil
}

func (l *Listener) Close() error {
	err := l.conn.Close()
	if err != nil {
		return &net.OpError{
			Op:   "close",
			Net:  "pcap",
			Addr: l.Addr(),
			Err:  err,
		}
	}

	return nil
}

// Dev returns the device.
func (l *Listener) Dev() *Device {
	return l.conn.LocalDev()
}

func (l *Listener) Addr() net.Addr {
	return &net.TCPAddr{
		IP:   l.Dev().IPAddr().IP,
		Port: int(l.srcPort),
	}
}

// DialWithKCP connects to the remote address in the pcap connection with KCP support.
func DialWithKCP(srcDev, dstDev *Device, srcPort uint16, dstAddr *net.TCPAddr, crypt crypto.Crypt, mtu int, timeout int, config *config.KCPConfig) (*kcp.UDPSession, error) {
	conn, err := Dial(srcDev, dstDev, srcPort, dstAddr, crypt, mtu, timeout)
	if err != nil {
		return nil, err
	}

	sess, err := kcp.NewConn(dstAddr.String(), nil, config.DataShard, config.ParityShard, conn)
	if err != nil {
		return nil, &net.OpError{
			Op:     "dial",
			Net:    "pcap",
			Source: conn.LocalAddr(),
			Addr:   conn.RemoteAddr(),
			Err:    fmt.Errorf("kcp: %w", err),
		}
	}

	// Tuning
	err = tuneKCP(sess, config)
	if err != nil {
		sess.Close()
		return nil, &net.OpError{
			Op:     "dial",
			Net:    "pcap",
			Source: conn.LocalAddr(),
			Addr:   conn.RemoteAddr(),
			Err:    fmt.Errorf("tune: %w", err),
		}
	}

	return sess, nil
}

// ListenWithKCP listens for incoming packets addressed to the local address in the pcap connection with KCP support.
func ListenWithKCP(srcDev, dstDev *Device, srcPort uint16, crypt crypto.Crypt, mtu int, config *config.KCPConfig) (*kcp.Listener, error) {
	conn, err := listenMulticast(srcDev, dstDev, srcPort, crypt, mtu)
	if err != nil {
		return nil, err
	}

	listener, err := kcp.ServeConn(nil, config.DataShard, config.ParityShard, conn)
	if err != nil {
		return nil, &net.OpError{
			Op:     "listen",
			Net:    "pcap",
			Source: conn.LocalAddr(),
			Err:    fmt.Errorf("kcp: %w", err),
		}
	}

	return listener, err
}

func tuneKCP(sess *kcp.UDPSession, config *config.KCPConfig) error {
	ok := sess.SetMtu(config.MTU)
	if !ok {
		return fmt.Errorf("cannot set mtu")
	}

	sess.SetWindowSize(config.SendWindow, config.RecvWindow)

	sess.SetACKNoDelay(config.ACKNoDelay)

	sess.SetNoDelay(btoi(config.NoDelay), config.Interval, config.Resend, config.NC)

	return nil
}

// TuneKCP tunes a KCP connection.
func TuneKCP(sess *kcp.UDPSession, config *config.KCPConfig) error {
	err := tuneKCP(sess, config)
	if err != nil {
		return &net.OpError{
			Op:     "tune",
			Net:    "pcap",
			Source: sess.LocalAddr(),
			Addr:   sess.RemoteAddr(),
			Err:    err,
		}
	}

	return nil
}

func btoi(b bool) int {
	if b {
		return 1
	}

	return 0
}
