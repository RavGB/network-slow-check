package main

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// listInterfaces returns every network interface with its state,
// MTU, and addresses.
func listInterfaces() []interfaceInfo {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	out := make([]interfaceInfo, 0, len(ifaces))
	for _, iface := range ifaces {
		info := interfaceInfo{
			Name:     iface.Name,
			Up:       iface.Flags&net.FlagUp != 0,
			Loopback: iface.Flags&net.FlagLoopback != 0,
			MTU:      iface.MTU,
		}
		addrs, err := iface.Addrs()
		if err == nil {
			for _, addr := range addrs {
				info.Addresses = append(info.Addresses, addr.String())
			}
		}
		out = append(out, info)
	}
	return out
}

// defaultGateway returns the default gateway address. On Linux it is
// read from /proc/net/route; on other systems the check is skipped
// and the second return value explains that.
func defaultGateway() (gateway string, note string) {
	if runtime.GOOS != "linux" {
		return "", "skipped (gateway reading is supported on Linux only)"
	}
	data, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return "", fmt.Sprintf("unavailable (%v)", err)
	}
	if gw, ok := parseDefaultGateway(string(data)); ok {
		return gw, ""
	}
	return "", "no default route found"
}

// parseDefaultGateway is a pure parser for the contents of
// /proc/net/route. Each line has the fields:
//
//	Iface Destination Gateway Flags RefCnt Use Metric Mask MTU Window IRTT
//
// A default route has Destination 00000000. The Gateway field is a
// hexadecimal IPv4 address stored little-endian, so 0101A8C0 means
// bytes C0 A8 01 01, that is 192.168.1.1. When several default routes
// exist, the one with the lowest metric wins.
func parseDefaultGateway(routeContents string) (string, bool) {
	bestMetric := -1
	var best string
	for _, line := range strings.Split(routeContents, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 8 {
			continue
		}
		if fields[1] != "00000000" { // Destination: not a default route
			continue
		}
		gatewayHex := fields[2]
		if gatewayHex == "00000000" { // no gateway (direct route)
			continue
		}
		value, err := strconv.ParseUint(gatewayHex, 16, 32)
		if err != nil {
			continue
		}
		var raw [4]byte
		binary.LittleEndian.PutUint32(raw[:], uint32(value))
		ip := net.IPv4(raw[0], raw[1], raw[2], raw[3])

		metric, err := strconv.Atoi(fields[6])
		if err != nil {
			metric = 0
		}
		if bestMetric == -1 || metric < bestMetric {
			bestMetric = metric
			best = ip.String()
		}
	}
	return best, best != ""
}

// dnsServers returns the configured DNS servers, read from
// /etc/resolv.conf where that file exists.
func dnsServers() []string {
	data, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		return nil
	}
	return parseResolvConf(string(data))
}

// parseResolvConf is a pure parser for resolv.conf contents. It
// returns the addresses from "nameserver" lines in order, without
// duplicates. Comments starting with # or ; and every other directive
// are ignored.
func parseResolvConf(contents string) []string {
	var servers []string
	seen := make(map[string]bool)
	for _, line := range strings.Split(contents, "\n") {
		// Drop full-line and trailing comments.
		if idx := strings.IndexAny(line, "#;"); idx >= 0 {
			line = line[:idx]
		}
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] != "nameserver" {
			continue
		}
		if !seen[fields[1]] {
			seen[fields[1]] = true
			servers = append(servers, fields[1])
		}
	}
	return servers
}
