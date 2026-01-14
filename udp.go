package imc

import (
	"context"
	"fmt"
	"net"
	"syscall"

	"golang.org/x/net/ipv4"
)

type Transporter struct {
	Proto *Protocol
	Conn  *net.UDPConn
}

func NewUDPTransporter(proto *Protocol, addr string) (*Transporter, error) {
	lc := net.ListenConfig{
		Control: func(network, address string, c syscall.RawConn) error {
			var err error
			c.Control(func(fd uintptr) {
				err = setSocketOptions(fd)
			})
			return err
		},
	}

	pc, err := lc.ListenPacket(context.Background(), "udp", addr)
	if err != nil {
		return nil, err
	}

	conn, ok := pc.(*net.UDPConn)
	if !ok {
		pc.Close()
		return nil, fmt.Errorf("could not get UDPConn")
	}

	return &Transporter{
		Proto: proto,
		Conn:  conn,
	}, nil
}

func (t *Transporter) JoinMulticast(groupAddr string) error {
	addr, err := net.ResolveUDPAddr("udp", groupAddr)
	if err != nil {
		return err
	}

	p := ipv4.NewPacketConn(t.Conn)

	// Join the multicast group on all interfaces
	ifaces, err := net.Interfaces()
	if err != nil {
		return err
	}

	joined := false
	for _, iface := range ifaces {
		if iface.Flags&net.FlagMulticast != 0 {
			if err := p.JoinGroup(&iface, addr); err == nil {
				joined = true
			}
		}
	}

	if !joined {
		return fmt.Errorf("failed to join multicast group %s on any interface", groupAddr)
	}
	return nil
}

func (t *Transporter) Send(msg *Message, addr string) error {
	data, err := msg.Serialize(t.Proto)
	if err != nil {
		return err
	}

	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return err
	}

	_, err = t.Conn.WriteToUDP(data, udpAddr)
	return err
}

func (t *Transporter) Receive() (*Message, *net.UDPAddr, error) {
	buf := make([]byte, 65535)
	n, addr, err := t.Conn.ReadFromUDP(buf)
	if err != nil {
		return nil, nil, err
	}

	msg, err := t.Proto.Unmarshal(buf[:n])
	if err != nil {
		return nil, addr, fmt.Errorf("failed to unmarshal message from %v: %w", addr, err)
	}

	return msg, addr, nil
}

func (t *Transporter) Close() error {
	return t.Conn.Close()
}
