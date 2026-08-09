package mtproto

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/gotd/td/telegram/dcs"
	xproxy "golang.org/x/net/proxy"
)

func newMTProtoProxyResolver(rawURL string) (dcs.Resolver, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse MTProto proxy URL: %w", err)
	}

	forward := &net.Dialer{
		Timeout:   15 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	dialer, err := xproxy.FromURL(parsed, forward)
	if err != nil {
		return nil, fmt.Errorf("initialize MTProto proxy: %w", err)
	}
	contextDialer, ok := dialer.(xproxy.ContextDialer)
	if !ok {
		return nil, fmt.Errorf("MTProto proxy does not support context-aware dialing")
	}

	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		return contextDialer.DialContext(ctx, network, address)
	}
	return dcs.Plain(dcs.PlainOptions{Dial: dial}), nil
}

func mtProtoProxyDescription(rawURL string) (scheme, address string) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "SOCKS5", "invalid"
	}
	scheme = strings.ToUpper(parsed.Scheme)
	address = parsed.Host
	if parsed.Port() == "" {
		address = net.JoinHostPort(parsed.Hostname(), "1080")
	}
	return scheme, address
}
