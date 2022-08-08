//go:build with_lwip

package tun

import (
	"context"
	"net"
	"net/netip"
	"os"
	"runtime"

	"github.com/sagernet/sing/common"
	"github.com/sagernet/sing/common/buf"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/common/udpnat"

	lwip "github.com/eycorsican/go-tun2socks/core"
)

type LWIP struct {
	ctx        context.Context
	tun        Tun
	tunMtu     uint32
	udpTimeout int64
	handler    Handler
	stack      lwip.LWIPStack
	udpNat     *udpnat.Service[netip.AddrPort]
}

func NewLWIP(
	ctx context.Context,
	tun Tun,
	tunMtu uint32,
	udpTimeout int64,
	handler Handler,
) (Stack, error) {
	return &LWIP{
		ctx:     ctx,
		tun:     tun,
		tunMtu:  tunMtu,
		handler: handler,
		stack:   lwip.NewLWIPStack(),
		udpNat:  udpnat.New[netip.AddrPort](udpTimeout, handler),
	}, nil
}

func (l *LWIP) Start() error {
	lwip.RegisterTCPConnHandler(l)
	lwip.RegisterUDPConnHandler(l)
	lwip.RegisterOutputFn(l.tun.Write)
	go l.loopIn()
	return nil
}

func (l *LWIP) loopIn() {
	mtu := int(l.tunMtu)
	if runtime.GOOS == "darwin" {
		mtu += 4
	}
	_buffer := buf.StackNewSize(mtu)
	defer common.KeepAlive(_buffer)
	buffer := common.Dup(_buffer)
	defer buffer.Release()
	data := buffer.FreeBytes()
	for {
		n, err := l.tun.Read(data)
		if err != nil {
			return
		}
		var packet []byte
		if runtime.GOOS == "darwin" {
			packet = data[4:n]
		} else {
			packet = data[:n]
		}
		_, err = l.stack.Write(packet)
		if err != nil {
			if err.Error() == "stack closed" {
				return
			}
			l.handler.NewError(context.Background(), err)
		}
	}
}

func (l *LWIP) Close() error {
	lwip.RegisterTCPConnHandler(nil)
	lwip.RegisterUDPConnHandler(nil)
	lwip.RegisterOutputFn(func(bytes []byte) (int, error) {
		return 0, os.ErrClosed
	})
	return l.stack.Close()
}

func (l *LWIP) Handle(conn net.Conn, target *net.TCPAddr) error {
	lAddr := conn.LocalAddr()
	rAddr := conn.RemoteAddr()
	if lAddr == nil || rAddr == nil {
		conn.Close()
		return nil
	}
	go func() {
		var metadata M.Metadata
		metadata.Source = M.SocksaddrFromNet(lAddr)
		metadata.Destination = M.SocksaddrFromNet(rAddr)
		hErr := l.handler.NewConnection(l.ctx, conn, metadata)
		if hErr != nil {
			conn.(lwip.TCPConn).Abort()
		}
	}()
	return nil
}

func (l *LWIP) Connect(conn lwip.UDPConn, target *net.UDPAddr) error {
	return nil
}

func (l *LWIP) ReceiveTo(conn lwip.UDPConn, data []byte, addr *net.UDPAddr) error {
	var upstreamMetadata M.Metadata
	upstreamMetadata.Source = M.SocksaddrFromNet(conn.LocalAddr())
	upstreamMetadata.Destination = M.SocksaddrFromNet(addr)

	l.udpNat.NewPacket(
		l.ctx,
		upstreamMetadata.Source.AddrPort(),
		buf.As(data).ToOwned(),
		upstreamMetadata,
		func(natConn N.PacketConn) N.PacketWriter {
			return &LWIPUDPBackWriter{conn}
		},
	)
	return nil
}

type LWIPUDPBackWriter struct {
	conn lwip.UDPConn
}

func (w *LWIPUDPBackWriter) WritePacket(buffer *buf.Buffer, destination M.Socksaddr) error {
	defer buffer.Release()
	return common.Error(w.conn.WriteFrom(buffer.Bytes(), destination.UDPAddr()))
}

func (w *LWIPUDPBackWriter) Close() error {
	return w.conn.Close()
}