package outbound

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing/common"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/protocol/socks"
	"github.com/sagernet/sing/protocol/socks/socks5"
)

type fallbackLanding struct {
	Server   string `json:"server"`
	Port     uint16 `json:"server_port"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

func (l fallbackLanding) address() M.Socksaddr {
	return M.ParseSocksaddr(net.JoinHostPort(l.Server, strconv.Itoa(int(l.Port))))
}

var errFallbackTargetRejected = errors.New("SOCKS5 landing rejected the target")

func dialFallbackSOCKS(ctx context.Context, front adapter.Outbound, landing, destination M.Socksaddr, user, password string) (net.Conn, error) {
	conn, err := front.DialContext(ctx, N.NetworkTCP, landing)
	if err != nil {
		return nil, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	result, err := socks.ClientHandshake5(conn, socks5.CommandConnect, destination, user, password)
	stop()
	if ctx.Err() != nil {
		_ = conn.Close()
		return nil, ctx.Err()
	}
	if err != nil {
		_ = conn.Close()
		if result.ReplyCode != socks5.ReplyCodeSuccess {
			return nil, fmt.Errorf("%w: %v", errFallbackTargetRejected, err)
		}
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

// fallbackSOCKS keeps the subscription's SOCKS node and its identity. For a
// tiered detour it retries the complete SOCKS CONNECT through another front.
type fallbackSOCKS struct {
	adapter.Outbound
	manager  *detourOutboundManager
	detour   string
	server   M.Socksaddr
	username string
	password string
}

func (o *fallbackSOCKS) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	if N.NetworkName(network) != N.NetworkTCP {
		return o.Outbound.DialContext(ctx, network, destination)
	}
	ob, _ := o.manager.Outbound(o.detour)
	if live, ok := ob.(*liveDetourGroup); ok {
		ob, _ = live.current()
	}
	group, ok := ob.(*fallback)
	if !ok || !group.tiered {
		return o.Outbound.DialContext(ctx, network, destination)
	}
	return group.dial(ctx, o.server, func(attempt context.Context, front adapter.Outbound) (net.Conn, error) {
		return dialFallbackSOCKS(attempt, front, o.server, destination, o.username, o.password)
	})
}

func (o *fallbackSOCKS) Close() error { return common.Close(o.Outbound) }
