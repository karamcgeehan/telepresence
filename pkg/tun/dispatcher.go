package tun

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"

	"google.golang.org/grpc"

	"github.com/telepresenceio/telepresence/v2/pkg/client"

	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
	"golang.org/x/sys/unix"

	"github.com/datawire/dlib/dgroup"
	"github.com/datawire/dlib/dlog"
	"github.com/telepresenceio/telepresence/rpc/v2/manager"
	"github.com/telepresenceio/telepresence/v2/pkg/connpool"
	"github.com/telepresenceio/telepresence/v2/pkg/tun/buffer"
	"github.com/telepresenceio/telepresence/v2/pkg/tun/icmp"
	"github.com/telepresenceio/telepresence/v2/pkg/tun/ip"
	"github.com/telepresenceio/telepresence/v2/pkg/tun/socks"
	"github.com/telepresenceio/telepresence/v2/pkg/tun/tcp"
	"github.com/telepresenceio/telepresence/v2/pkg/tun/udp"
)

type Dispatcher struct {
	dev            *Device
	socksTCPDialer socks.Dialer
	managerClient  manager.ManagerClient
	udpStream      *udp.Stream
	udpHandlers    *connpool.Pool
	tcpHandlers    *connpool.Pool
	handlersWg     sync.WaitGroup
	toTunCh        chan ip.Packet
	fragmentMap    map[uint16][]*buffer.Data
	closing        int32
	dialersSet     chan struct{}
}

func NewDispatcher(dev *Device) *Dispatcher {
	return &Dispatcher{
		dev:         dev,
		udpHandlers: connpool.NewPool(),
		tcpHandlers: connpool.NewPool(),
		toTunCh:     make(chan ip.Packet, 100),
		dialersSet:  make(chan struct{}),
		fragmentMap: make(map[uint16][]*buffer.Data),
	}
}

var closeDialers = sync.Once{}

func (d *Dispatcher) SetPorts(ctx context.Context, socksPort, managerPort uint16) (err error) {
	if d.socksTCPDialer == nil || d.socksTCPDialer.ProxyPort() != socksPort {
		if d.socksTCPDialer, err = socks.Proxy.NewDialer(ctx, "tcp", socksPort); err != nil {
			return err
		}
	}
	if managerPort != 0 && d.managerClient == nil {
		// First check. Establish connection
		tos := &client.GetConfig(ctx).Timeouts
		tc, cancel := context.WithTimeout(ctx, tos.TrafficManagerAPI)
		defer cancel()

		var conn *grpc.ClientConn
		conn, err = grpc.DialContext(tc, fmt.Sprintf("127.0.0.1:%d", managerPort),
			grpc.WithInsecure(),
			grpc.WithNoProxy(),
			grpc.WithBlock())
		if err != nil {
			return client.CheckTimeout(tc, &tos.TrafficManagerAPI, err)
		}
		d.managerClient = manager.NewManagerClient(conn)
	}
	closeDialers.Do(func() { close(d.dialersSet) })
	return nil
}

func (d *Dispatcher) Stop(c context.Context) {
	atomic.StoreInt32(&d.closing, 1)
	d.udpHandlers.CloseAll(c)
	d.tcpHandlers.CloseAll(c)
	d.handlersWg.Wait()
	atomic.StoreInt32(&d.closing, 2)
	d.dev.Close()
}

var blockedUDPPorts = map[uint16]bool{
	137: true, // NETBIOS Name Service
	138: true, // NETBIOS Datagram Service
	139: true, // NETBIOS
}

func (d *Dispatcher) Run(c context.Context) error {
	g := dgroup.NewGroup(c, dgroup.GroupConfig{})

	// writer
	g.Go("TUN writer", func(c context.Context) error {
		for atomic.LoadInt32(&d.closing) < 2 {
			select {
			case <-c.Done():
				return nil
			case pkt := <-d.toTunCh:
				// dlog.Debugf(c, "-> TUN: %s", pkt)
				_, err := d.dev.Write(pkt.Data())
				pkt.SoftRelease()
				if err != nil {
					if atomic.LoadInt32(&d.closing) == 2 || c.Err() != nil {
						err = nil
					}
					return err
				}
			}
		}
		return nil
	})

	g.Go("MGR udp-stream", func(c context.Context) error {
		// Block here until socks dialers are configured
		select {
		case <-c.Done():
			return nil
		case <-d.dialersSet:
		}

		if d.managerClient != nil {
			// TODO: UDPTunnel should probably provide a sessionID
			udpStream, err := d.managerClient.UDPTunnel(c)
			if err != nil {
				return err
			}
			d.udpStream = udp.NewStream(udpStream, d.toTunCh)
			return d.udpStream.ReadLoop(c)
		}
		return nil
	})

	g.Go("TUN reader", func(c context.Context) error {
		// Block here until socks dialers are configured
		select {
		case <-c.Done():
			return nil
		case <-d.dialersSet:
		}

		for atomic.LoadInt32(&d.closing) < 2 {
			data := buffer.DataPool.Get(buffer.DataPool.MTU)
			for {
				n, err := d.dev.Read(data)
				if err != nil {
					buffer.DataPool.Put(data)
					if c.Err() != nil || atomic.LoadInt32(&d.closing) == 2 {
						return nil
					}
					return fmt.Errorf("read packet error: %v", err)
				}
				if n > 0 {
					data.SetLength(n)
					break
				}
			}
			d.handlePacket(c, data)
		}
		return nil
	})
	return g.Wait()
}

func (d *Dispatcher) handlePacket(c context.Context, data *buffer.Data) {
	defer func() {
		if data != nil {
			buffer.DataPool.Put(data)
		}
	}()

	hdr, err := ip.ParseHeader(data.Buf())
	if err != nil {
		dlog.Error(c, "Unable to parse package header")
		return
	}

	if hdr.PayloadLen() > buffer.DataPool.MTU-hdr.HeaderLen() {
		// Package is too large for us.
		d.toTunCh <- icmp.DestinationUnreachablePacket(uint16(buffer.DataPool.MTU), hdr, icmp.MustFragment)
		return
	}

	if hdr.Version() == ipv6.Version {
		dlog.Error(c, "IPv6 is not yet handled by this dispatcher")
		d.toTunCh <- icmp.DestinationUnreachablePacket(uint16(buffer.DataPool.MTU), hdr, 0) // 0 == ICMPv6 code "no route to destination"
		return
	}

	ipHdr := hdr.(ip.V4Header)
	if ipHdr.Flags()&ipv4.MoreFragments != 0 || ipHdr.FragmentOffset() != 0 {
		data = ipHdr.ConcatFragments(data, d.fragmentMap)
		if data == nil {
			return
		}
		ipHdr = data.Buf()
	}

	switch ipHdr.L4Protocol() {
	case unix.IPPROTO_TCP:
		d.tcp(c, tcp.MakePacket(ipHdr, data))
		data = nil
	case unix.IPPROTO_UDP:
		if d.udpStream == nil {
			d.toTunCh <- icmp.DestinationUnreachablePacket(uint16(buffer.DataPool.MTU), hdr, icmp.ProtocolUnreachable)
			return
		}
		dst := ipHdr.Destination()
		if dst.IsLinkLocalUnicast() || dst.IsLinkLocalMulticast() {
			// Just ignore at this point.
			return
		}
		if ip4 := dst.To4(); ip4 != nil && ip4[2] == 0 && ip4[3] == 0 {
			// Write to the a subnet's zero address. Not sure why this is happening but there's no point in
			// passing them on.
			d.toTunCh <- icmp.DestinationUnreachablePacket(uint16(buffer.DataPool.MTU), hdr, icmp.HostUnreachable)
			return
		}
		dg := udp.MakeDatagram(ipHdr, data)
		if blockedUDPPorts[dg.Header().SourcePort()] || blockedUDPPorts[dg.Header().DestinationPort()] {
			d.toTunCh <- icmp.DestinationUnreachablePacket(uint16(buffer.DataPool.MTU), hdr, icmp.PortUnreachable)
			return
		}
		data = nil
		d.udp(c, dg)
	case unix.IPPROTO_ICMP:
	case unix.IPPROTO_ICMPV6:
		pkt := icmp.MakePacket(ipHdr, data)
		dlog.Debugf(c, "<- TUN %s", pkt)
	default:
		// An L4 protocol that we don't handle.
		d.toTunCh <- icmp.DestinationUnreachablePacket(uint16(buffer.DataPool.MTU), hdr, icmp.ProtocolUnreachable)
	}
}

func (d *Dispatcher) tcp(c context.Context, pkt tcp.Packet) {
	// dlog.Debugf(c, "<- TUN %s", pkt)
	ipHdr := pkt.IPHeader()
	tcpHdr := pkt.Header()
	connID := connpool.NewConnID(ipHdr.Source(), ipHdr.Destination(), tcpHdr.SourcePort(), tcpHdr.DestinationPort())
	wf, err := d.tcpHandlers.Get(c, connID, func(c context.Context, remove func()) (connpool.Handler, error) {
		if tcpHdr.RST() {
			return nil, errors.New("dispatching got RST without connection workflow")
		}
		if !tcpHdr.SYN() {
			select {
			case <-c.Done():
				return nil, c.Err()
			case d.toTunCh <- pkt.Reset():
			}
		}
		wf := tcp.NewWorkflow(d.socksTCPDialer, &d.closing, d.toTunCh, connID, remove)
		d.handlersWg.Add(1)
		go wf.Run(c, &d.handlersWg)
		return wf, nil
	})
	if err != nil {
		dlog.Error(c, err)
		return
	}
	wf.(*tcp.Workflow).NewPacket(c, pkt)
}

func (d *Dispatcher) udp(c context.Context, dg udp.Datagram) {
	dlog.Debugf(c, "<- TUN %s", dg)
	ipHdr := dg.IPHeader()
	udpHdr := dg.Header()
	connID := connpool.NewConnID(ipHdr.Source(), ipHdr.Destination(), udpHdr.SourcePort(), udpHdr.DestinationPort())
	uh, err := d.udpHandlers.Get(c, connID, func(c context.Context, release func()) (connpool.Handler, error) {
		handler := udp.NewHandler(d.udpStream, connID, release)
		d.handlersWg.Add(1)
		go handler.Run(c, &d.handlersWg)
		return handler, nil
	})
	if err != nil {
		dlog.Error(c, err)
		return
	}
	uh.(*udp.Handler).NewDatagram(c, dg)
}

func (d *Dispatcher) AddSubnets(c context.Context, subnets []*net.IPNet) error {
	for _, sn := range subnets {
		to := make(net.IP, len(sn.IP))
		copy(to, sn.IP)
		to[len(to)-1] = 1
		if err := d.dev.AddSubnet(c, sn, to); err != nil {
			return err
		}
	}
	return nil
}
