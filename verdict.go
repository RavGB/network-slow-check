package main

import "time"

// Verdict thresholds. Averages above these values count as slow.
// They are deliberately plain, everyday-network numbers: a healthy
// local network answers DNS and opens connections well under them,
// so crossing one is a real signal rather than noise.
//
//   - DNS: home and office resolvers usually answer in 10-50 ms.
//     Averaging over 200 ms means lookups alone add a visible delay
//     to every new connection.
//   - TCP connect: one round trip plus setup. Over 300 ms on average
//     points at the path (weak signal, congestion, a long route).
//   - Time to first byte: over 800 ms while connecting is fast means
//     the far end is slow to start answering.
//   - Loopback: a round trip on the same device is normally well
//     under 1 ms. Over 5 ms means the device itself is struggling.
//   - Download: under 1 MB/s (about 8 Mbit/s) is slow for everyday
//     browsing and updates on a modern connection.
const (
	dnsSlowAvg      = 200 * time.Millisecond
	tcpSlowAvg      = 300 * time.Millisecond
	ttfbSlow        = 800 * time.Millisecond
	loopbackSlowAvg = 5 * time.Millisecond
	downloadSlowMBs = 1.0
)

// findings is the reduced set of numbers the verdict rules read.
type findings struct {
	DNSAvg         time.Duration
	DNSFailed      bool
	TCPAvg         time.Duration
	TCPFailed      bool
	HasHTTP        bool
	HTTPFailed     bool
	TTFB           time.Duration
	HasLoopback    bool
	LoopbackFailed bool
	LoopbackAvg    time.Duration
	HasDownload    bool
	DownloadFailed bool
	DownloadMBps   float64
}

// verdict turns findings into plain likely-cause lines. The second
// return value is true when anything was slow or failed. Pure
// function, table tested.
func verdict(f findings) (lines []string, slow bool) {
	dnsSlow := f.DNSAvg > dnsSlowAvg
	tcpSlow := f.TCPAvg > tcpSlowAvg

	if f.DNSFailed {
		lines = append(lines, "Some DNS lookups failed outright. Check the DNS servers listed above and whether name resolution works at all before anything else.")
		slow = true
	} else if dnsSlow {
		lines = append(lines, "DNS lookups are slow on average, so lookups are the bottleneck: every new connection waits on a slow name lookup first. Try another DNS server to compare.")
		slow = true
	}

	if f.TCPFailed {
		lines = append(lines, "Some TCP connections failed outright. The device can resolve names but cannot reliably open connections, which points at the local link or the gateway.")
		slow = true
	} else if tcpSlow && !dnsSlow {
		lines = append(lines, "Opening connections is slow while name lookups are fast, so the problem is the path or the link itself (weak signal, congestion, or a long route), not DNS.")
		slow = true
	} else if tcpSlow && dnsSlow {
		lines = append(lines, "Opening connections is slow as well as DNS, which fits a weak or congested link making every round trip slow.")
		slow = true
	}

	if f.HasHTTP {
		if f.HTTPFailed {
			lines = append(lines, "The HTTP request failed. Combined with the checks above, treat this as a connection problem rather than a slow website.")
			slow = true
		} else if f.TTFB > ttfbSlow && !tcpSlow {
			lines = append(lines, "Connections open quickly but the first byte takes a long time, so the far end is slow to respond. The local network looks fine; the delay sits with the site or service.")
			slow = true
		}
	}

	if f.HasLoopback {
		if f.LoopbackFailed {
			lines = append(lines, "Even the loopback test on this device failed, so check the device itself first (very high load, a stuck network stack, or security software in the way).")
			slow = true
		} else if f.LoopbackAvg > loopbackSlowAvg {
			lines = append(lines, "Even talking to itself, this device is slow, so the device itself is under load. Close heavy programs and run the check again before blaming the network.")
			slow = true
		}
	}

	if f.HasDownload {
		if f.DownloadFailed {
			lines = append(lines, "The download test failed, so throughput could not be measured. Treat the failures above as the main signal.")
			slow = true
		} else if f.DownloadMBps < downloadSlowMBs {
			lines = append(lines, "Measured download speed is under 1 MB/s, which is slow for everyday use. If the other checks are fast, the bottleneck is likely the internet connection or the gateway.")
			slow = true
		}
	}

	if len(lines) == 0 {
		lines = append(lines, "Nothing measured as slow and nothing failed. No local cause found: DNS, connections, first response, and this device all look healthy.")
	}
	return lines, slow
}
