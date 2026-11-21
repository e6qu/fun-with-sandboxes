package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sort"
	"strings"

	"golang.org/x/net/dns/dnsmessage"
)

// dnsServe answers every A question with addr, on port 53 over UDP and
// TCP, as a DNS server a container runs.
func dnsServe(addr string) error {
	a, err := netip.ParseAddr(addr)
	if err != nil || !a.Is4() {
		return fmt.Errorf("dnsserve: %q is no IPv4 address", addr)
	}
	answer := func(q []byte) []byte {
		var m dnsmessage.Message
		if m.Unpack(q) != nil || len(m.Questions) != 1 {
			return nil
		}
		m.Response, m.Authoritative = true, true
		if m.Questions[0].Type == dnsmessage.TypeA {
			h := dnsmessage.ResourceHeader{Name: m.Questions[0].Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 60}
			m.Answers = []dnsmessage.Resource{{Header: h, Body: &dnsmessage.AResource{A: a.As4()}}}
		}
		b, _ := m.Pack()
		return b
	}
	u, err := net.ListenPacket("udp", ":53")
	if err != nil {
		return err
	}
	l, err := net.Listen("tcp", ":53")
	if err != nil {
		return err
	}
	go func() {
		buf := make([]byte, 512)
		for {
			n, from, err := u.ReadFrom(buf)
			if err != nil {
				return
			}
			if b := answer(buf[:n]); b != nil {
				u.WriteTo(b, from)
			}
		}
	}()
	fmt.Println("serving DNS")
	for {
		c, err := l.Accept()
		if err != nil {
			return err
		}
		go func() {
			defer c.Close()
			var n [2]byte
			if _, err := io.ReadFull(c, n[:]); err != nil {
				return
			}
			q := make([]byte, binary.BigEndian.Uint16(n[:]))
			if _, err := io.ReadFull(c, q); err != nil {
				return
			}
			if b := answer(q); b != nil {
				c.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(b))), b...))
			}
		}()
	}
}

// lookup prints name's addresses as the system's resolver and Go's own,
// which sends DNS itself, find them.
func lookup(name string) error {
	var out []string
	for _, r := range []*net.Resolver{net.DefaultResolver, {PreferGo: true}} {
		addrs, err := r.LookupHost(context.Background(), name)
		if err != nil {
			return err
		}
		sort.Strings(addrs)
		out = append(out, strings.Join(addrs, ","))
	}
	fmt.Println(strings.Join(out, " "))
	return nil
}
