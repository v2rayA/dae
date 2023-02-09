/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) since 2023, mzz2017 <mzz@tuta.io>
 */

package dialer

import (
	"context"
	"errors"
	"fmt"
	"github.com/mzz2017/softwind/pkg/fastrand"
	"github.com/sirupsen/logrus"
	"github.com/v2rayA/dae/common/consts"
	"github.com/v2rayA/dae/common/netutils"
	"golang.org/x/net/dns/dnsmessage"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

type collection struct {
	// AliveDialerSetSet uses reference counting.
	AliveDialerSetSet AliveDialerSetSet
	Latencies10       *LatenciesN
	Alive             bool
}

func newCollection() *collection {
	return &collection{
		AliveDialerSetSet: make(AliveDialerSetSet),
		Latencies10:       NewLatenciesN(10),
		Alive:             true,
	}
}

func (d *Dialer) mustGetCollection(l4proto consts.L4ProtoStr, ipversion consts.IpVersionStr) *collection {
	switch l4proto {
	case consts.L4ProtoStr_TCP:
		switch ipversion {
		case consts.IpVersionStr_4:
			return d.collections[0]
		case consts.IpVersionStr_6:
			return d.collections[1]
		}
	case consts.L4ProtoStr_UDP:
		switch ipversion {
		case consts.IpVersionStr_4:
			return d.collections[2]
		case consts.IpVersionStr_6:
			return d.collections[3]
		}
	}
	panic("invalid param")
}

func (d *Dialer) MustGetLatencies10(l4proto consts.L4ProtoStr, ipversion consts.IpVersionStr) *LatenciesN {
	return d.mustGetCollection(l4proto, ipversion).Latencies10
}

func (d *Dialer) MustGetAlive(l4proto consts.L4ProtoStr, ipversion consts.IpVersionStr) bool {
	return d.mustGetCollection(l4proto, ipversion).Alive
}

type TcpCheckOption struct {
	Url *netutils.URL
	*netutils.Ip46
}

func ParseTcpCheckOption(ctx context.Context, rawURL string) (opt *TcpCheckOption, err error) {
	systemDns, err := netutils.SystemDns()
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = netutils.TryUpdateSystemDns1s()
		}
	}()

	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	ip46, err := netutils.ParseIp46(ctx, SymmetricDirect, systemDns, u.Hostname(), true)
	if err != nil {
		return nil, err
	}
	return &TcpCheckOption{
		Url:  &netutils.URL{URL: u},
		Ip46: ip46,
	}, nil
}

type UdpCheckOption struct {
	DnsHost string
	DnsPort uint16
	*netutils.Ip46
}

func ParseUdpCheckOption(ctx context.Context, dnsHostPort string) (opt *UdpCheckOption, err error) {
	systemDns, err := netutils.SystemDns()
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = netutils.TryUpdateSystemDns1s()
		}
	}()

	host, _port, err := net.SplitHostPort(dnsHostPort)
	if err != nil {
		return nil, err
	}
	port, err := strconv.ParseUint(_port, 10, 16)
	if err != nil {
		return nil, fmt.Errorf("bad port: %v", err)
	}
	ip46, err := netutils.ParseIp46(ctx, SymmetricDirect, systemDns, host, true)
	if err != nil {
		return nil, err
	}
	return &UdpCheckOption{
		DnsHost: host,
		DnsPort: uint16(port),
		Ip46:    ip46,
	}, nil
}

type TcpCheckOptionRaw struct {
	opt *TcpCheckOption
	mu  sync.Mutex
	Raw string
}

func (c *TcpCheckOptionRaw) Option() (opt *TcpCheckOption, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.opt == nil {
		ctx, cancel := context.WithTimeout(context.TODO(), 10*time.Second)
		defer cancel()
		tcpCheckOption, err := ParseTcpCheckOption(ctx, c.Raw)
		if err != nil {
			return nil, fmt.Errorf("failed to parse tcp_check_url: %w", err)
		}
		c.opt = tcpCheckOption
	}
	return c.opt, nil
}

type UdpCheckOptionRaw struct {
	opt *UdpCheckOption
	mu  sync.Mutex
	Raw string
}

func (c *UdpCheckOptionRaw) Option() (opt *UdpCheckOption, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.opt == nil {
		ctx, cancel := context.WithTimeout(context.TODO(), 10*time.Second)
		defer cancel()
		udpCheckOption, err := ParseUdpCheckOption(ctx, c.Raw)
		if err != nil {
			return nil, fmt.Errorf("failed to parse tcp_check_url: %w", err)
		}
		c.opt = udpCheckOption
	}
	return c.opt, nil
}

type CheckOption struct {
	L4proto   consts.L4ProtoStr
	IpVersion consts.IpVersionStr
	CheckFunc func(ctx context.Context) (ok bool, err error)
}

func (d *Dialer) ActivateCheck() {
	d.tickerMu.Lock()
	defer d.tickerMu.Unlock()
	if d.instanceOption.CheckEnabled {
		return
	}
	d.instanceOption.CheckEnabled = true
	go d.aliveBackground()
}

func (d *Dialer) aliveBackground() {
	timeout := 10 * time.Second
	cycle := d.CheckInterval
	tcp4CheckOpt := &CheckOption{
		L4proto:   consts.L4ProtoStr_TCP,
		IpVersion: consts.IpVersionStr_4,
		CheckFunc: func(ctx context.Context) (ok bool, err error) {
			opt, err := d.TcpCheckOptionRaw.Option()
			if err != nil {
				return false, err
			}
			return d.HttpCheck(ctx, opt.Url, opt.Ip4)
		},
	}
	tcp6CheckOpt := &CheckOption{
		L4proto:   consts.L4ProtoStr_TCP,
		IpVersion: consts.IpVersionStr_6,
		CheckFunc: func(ctx context.Context) (ok bool, err error) {
			opt, err := d.TcpCheckOptionRaw.Option()
			if err != nil {
				return false, err
			}
			return d.HttpCheck(ctx, opt.Url, opt.Ip6)
		},
	}
	udp4CheckOpt := &CheckOption{
		L4proto:   consts.L4ProtoStr_UDP,
		IpVersion: consts.IpVersionStr_4,
		CheckFunc: func(ctx context.Context) (ok bool, err error) {
			opt, err := d.UdpCheckOptionRaw.Option()
			if err != nil {
				return false, err
			}
			return d.DnsCheck(ctx, netip.AddrPortFrom(opt.Ip4, opt.DnsPort))
		},
	}
	udp6CheckOpt := &CheckOption{
		L4proto:   consts.L4ProtoStr_UDP,
		IpVersion: consts.IpVersionStr_6,
		CheckFunc: func(ctx context.Context) (ok bool, err error) {
			opt, err := d.UdpCheckOptionRaw.Option()
			if err != nil {
				return false, err
			}
			return d.DnsCheck(ctx, netip.AddrPortFrom(opt.Ip6, opt.DnsPort))
		},
	}
	// Check once immediately.
	go d.Check(timeout, tcp4CheckOpt)
	go d.Check(timeout, udp4CheckOpt)
	go d.Check(timeout, tcp6CheckOpt)
	go d.Check(timeout, udp6CheckOpt)

	ctx, cancel := context.WithCancel(d.ctx)
	defer cancel()
	go func() {
		/// Splice ticker.C to checkCh.
		// Sleep to avoid avalanche.
		time.Sleep(time.Duration(fastrand.Int63n(int64(cycle))))
		d.tickerMu.Lock()
		d.ticker = time.NewTicker(cycle)
		d.tickerMu.Unlock()
		for t := range d.ticker.C {
			select {
			case <-ctx.Done():
				return
			default:
				d.checkCh <- t
			}
		}
	}()
	var wg sync.WaitGroup
	for range d.checkCh {
		// No need to test if there is no dialer selection policy using its latency.
		if len(d.mustGetCollection(consts.L4ProtoStr_TCP, consts.IpVersionStr_4).AliveDialerSetSet) > 0 {
			wg.Add(1)
			go func() {
				d.Check(timeout, tcp4CheckOpt)
				wg.Done()
			}()
		}
		if len(d.mustGetCollection(consts.L4ProtoStr_TCP, consts.IpVersionStr_6).AliveDialerSetSet) > 0 {
			wg.Add(1)
			go func() {
				d.Check(timeout, tcp6CheckOpt)
				wg.Done()
			}()
		}
		if len(d.mustGetCollection(consts.L4ProtoStr_UDP, consts.IpVersionStr_4).AliveDialerSetSet) > 0 {
			wg.Add(1)
			go func() {
				d.Check(timeout, udp4CheckOpt)
				wg.Done()
			}()
		}
		if len(d.mustGetCollection(consts.L4ProtoStr_UDP, consts.IpVersionStr_6).AliveDialerSetSet) > 0 {
			wg.Add(1)
			go func() {
				d.Check(timeout, udp6CheckOpt)
				wg.Done()
			}()
		}
		// Wait to block the loop.
		wg.Wait()
	}
}

// NotifyCheck will succeed only when CheckEnabled is true.
func (d *Dialer) NotifyCheck() {
	select {
	case <-d.ctx.Done():
		return
	default:
	}

	select {
	// If fail to push elem to chan, the check is in process.
	case d.checkCh <- time.Now():
	default:
	}
}

// RegisterAliveDialerSet is thread-safe.
func (d *Dialer) RegisterAliveDialerSet(a *AliveDialerSet, l4proto consts.L4ProtoStr, ipversion consts.IpVersionStr) {
	d.collectionFineMu.Lock()
	d.mustGetCollection(l4proto, ipversion).AliveDialerSetSet[a]++
	d.collectionFineMu.Unlock()
}

// UnregisterAliveDialerSet is thread-safe.
func (d *Dialer) UnregisterAliveDialerSet(a *AliveDialerSet, l4proto consts.L4ProtoStr, ipversion consts.IpVersionStr) {
	d.collectionFineMu.Lock()
	defer d.collectionFineMu.Unlock()
	setSet := d.mustGetCollection(consts.L4ProtoStr_TCP, consts.IpVersionStr_4).AliveDialerSetSet
	setSet[a]--
	if setSet[a] <= 0 {
		delete(setSet, a)
	}
}

func (d *Dialer) Check(timeout time.Duration,
	opts *CheckOption,
) (ok bool, err error) {
	ctx, cancel := context.WithTimeout(context.TODO(), timeout)
	defer cancel()
	start := time.Now()
	// Calc latency.
	collection := d.mustGetCollection(opts.L4proto, opts.IpVersion)
	if ok, err = opts.CheckFunc(ctx); ok && err == nil {
		// No error.
		latency := time.Since(start)
		latencies10 := d.mustGetCollection(opts.L4proto, opts.IpVersion).Latencies10
		latencies10.AppendLatency(latency)
		avg, _ := latencies10.AvgLatency()
		d.Log.WithFields(logrus.Fields{
			// Add a space to ensure alphabetical order is first.
			"network": string(opts.L4proto) + string(opts.IpVersion),
			"node":    d.name,
			"last":    latency.Truncate(time.Millisecond),
			"avg_10":  avg.Truncate(time.Millisecond),
		}).Debugln("Connectivity Check")
		collection.Alive = true
	} else {
		// Append timeout if there is any error or unexpected status code.
		if err != nil {
			if strings.Contains(err.Error(), "network is unreachable") {
				err = fmt.Errorf("network is unreachable")
			}
			d.Log.WithFields(logrus.Fields{
				// Add a space to ensure alphabetical order is first.
				"network": string(opts.L4proto) + string(opts.IpVersion),
				"node":    d.name,
				"err":     err.Error(),
			}).Debugln("Connectivity Check Failed")
		}
		latencies10 := collection.Latencies10
		latencies10.AppendLatency(timeout)
		collection.Alive = false
	}
	// Inform DialerGroups to update state.
	// We use lock because AliveDialerSetSet is a reference of that in collection.
	d.collectionFineMu.Lock()
	for a := range collection.AliveDialerSetSet {
		a.NotifyLatencyChange(d, collection.Alive)
	}
	d.collectionFineMu.Unlock()
	return ok, err
}

func (d *Dialer) HttpCheck(ctx context.Context, u *netutils.URL, ip netip.Addr) (ok bool, err error) {
	// HTTP(S) check.
	cd := netutils.ContextDialer{d.Dialer}
	cli := http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (c net.Conn, err error) {
				// Force to dial "ip".
				return cd.DialContext(ctx, "tcp", net.JoinHostPort(ip.String(), u.Port()))
			},
		},
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return false, err
	}
	resp, err := cli.Do(req)
	if err != nil {
		var netErr net.Error
		if errors.As(err, &netErr); netErr.Timeout() {
			err = fmt.Errorf("timeout")
		}
		return false, err
	}
	defer resp.Body.Close()
	// Judge the status code.
	if page := path.Base(req.URL.Path); strings.HasPrefix(page, "generate_") {
		return strconv.Itoa(resp.StatusCode) == strings.TrimPrefix(page, "generate_"), nil
	}
	return resp.StatusCode >= 200 && resp.StatusCode < 400, nil
}

func (d *Dialer) DnsCheck(ctx context.Context, dns netip.AddrPort) (ok bool, err error) {
	addrs, err := netutils.ResolveNetip(ctx, d, dns, consts.UdpCheckLookupHost, dnsmessage.TypeA)
	if err != nil {
		return false, err
	}
	if len(addrs) == 0 {
		return false, fmt.Errorf("bad DNS response: no record")
	}
	return true, nil
}