package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

// prober は 1 回分のチェックを行い、成功時は応答時間を返す。
type prober func(ctx context.Context) (time.Duration, error)

func newProber(t *Target) prober {
	switch t.Type {
	case "icmp":
		return func(ctx context.Context) (time.Duration, error) {
			return probeICMP(ctx, t.Host, t.ICMPCount, t.Timeout)
		}
	case "tcp":
		return func(ctx context.Context) (time.Duration, error) {
			return probeTCP(ctx, t.Host, t.Timeout)
		}
	case "http":
		return newHTTPProber(t)
	}
	panic("unknown target type " + t.Type)
}

var warnUnprivileged sync.Once

// listenICMP は raw ソケット (CAP_NET_RAW が必要) を優先し、使えなければ
// unprivileged ICMP (net.ipv4.ping_group_range が必要) にフォールバックする。
func listenICMP(v6 bool) (conn *icmp.PacketConn, raw bool, err error) {
	rawNet, udpNet, addr := "ip4:icmp", "udp4", "0.0.0.0"
	if v6 {
		rawNet, udpNet, addr = "ip6:ipv6-icmp", "udp6", "::"
	}
	conn, rawErr := icmp.ListenPacket(rawNet, addr)
	if rawErr == nil {
		return conn, true, nil
	}
	conn, err = icmp.ListenPacket(udpNet, addr)
	if err != nil {
		return nil, false, fmt.Errorf("open icmp socket: %v / %v (grant CAP_NET_RAW: VyOS 'capability net-raw')", rawErr, err)
	}
	warnUnprivileged.Do(func() {
		slog.Warn("raw ICMP socket unavailable, using unprivileged ping", "err", rawErr)
	})
	return conn, false, nil
}

// probeICMP は count 回 echo request を送り、1 つでも応答があれば成功とする。
func probeICMP(ctx context.Context, host string, count int, timeout time.Duration) (time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ip, err := resolveIP(ctx, host)
	if err != nil {
		return 0, err
	}
	v6 := ip.To4() == nil
	conn, raw, err := listenICMP(v6)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	conn.SetDeadline(deadline)
	defer context.AfterFunc(ctx, func() { conn.SetDeadline(time.Now()) })()

	// unprivileged ソケットではカーネルが ID を書き換えるため、payload 内のトークンで照合する
	var token [8]byte
	rand.Read(token[:])
	id := int(binary.BigEndian.Uint16(token[:2]))

	var reqType, replyType icmp.Type = ipv4.ICMPTypeEcho, ipv4.ICMPTypeEchoReply
	proto := 1
	if v6 {
		reqType, replyType, proto = ipv6.ICMPTypeEchoRequest, ipv6.ICMPTypeEchoReply, 58
	}
	var dst net.Addr = &net.IPAddr{IP: ip}
	if !raw {
		dst = &net.UDPAddr{IP: ip}
	}

	sendErr := make(chan error, 1)
	go func() {
		gap := timeout / time.Duration(count+1)
		for seq := 1; seq <= count; seq++ {
			payload := make([]byte, 16)
			binary.BigEndian.PutUint64(payload, uint64(time.Now().UnixNano()))
			copy(payload[8:], token[:])
			msg := icmp.Message{Type: reqType, Body: &icmp.Echo{ID: id, Seq: seq, Data: payload}}
			b, _ := msg.Marshal(nil)
			if _, err := conn.WriteTo(b, dst); err != nil {
				sendErr <- err
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(gap):
			}
		}
	}()

	buf := make([]byte, 1500)
	for {
		n, _, err := conn.ReadFrom(buf)
		if err != nil {
			select {
			case se := <-sendErr:
				return 0, fmt.Errorf("send echo to %s: %w", ip, se)
			default:
			}
			return 0, fmt.Errorf("no echo reply from %s (%d sent, timeout %s)", ip, count, timeout)
		}
		msg, err := icmp.ParseMessage(proto, buf[:n])
		if err != nil || msg.Type != replyType {
			continue
		}
		echo, ok := msg.Body.(*icmp.Echo)
		if !ok || len(echo.Data) < 16 || !bytes.Equal(echo.Data[8:16], token[:]) {
			continue
		}
		sent := time.Unix(0, int64(binary.BigEndian.Uint64(echo.Data[:8])))
		return time.Since(sent), nil
	}
}

func resolveIP(ctx context.Context, host string) (net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		return ip, nil
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	for _, a := range addrs {
		if a.IP.To4() != nil {
			return a.IP, nil
		}
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("no address for %s", host)
	}
	return addrs[0].IP, nil
}

func probeTCP(ctx context.Context, addr string, timeout time.Duration) (time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return 0, err
	}
	conn.Close()
	return time.Since(start), nil
}

func newHTTPProber(t *Target) prober {
	client := &http.Client{
		Timeout: t.Timeout,
		Transport: &http.Transport{
			// 管理画面は自己署名証明書が多いため、ターゲットごとに検証を無効化できるようにする
			TLSClientConfig:   &tls.Config{InsecureSkipVerify: t.InsecureSkipVerify},
			DisableKeepAlives: true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return func(ctx context.Context) (time.Duration, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.URL, nil)
		if err != nil {
			return 0, err
		}
		req.Header.Set("User-Agent", "pingcord/"+version)
		start := time.Now()
		resp, err := client.Do(req)
		if err != nil {
			return 0, err
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		rtt := time.Since(start)

		if t.ExpectStatus != 0 {
			if resp.StatusCode != t.ExpectStatus {
				return 0, fmt.Errorf("HTTP %d (expected %d)", resp.StatusCode, t.ExpectStatus)
			}
		} else if resp.StatusCode >= 400 {
			return 0, fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		return rtt, nil
	}
}
