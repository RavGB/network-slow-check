package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSummarize(t *testing.T) {
	tests := []struct {
		name          string
		samples       []time.Duration
		min, avg, max time.Duration
	}{
		{"empty", nil, 0, 0, 0},
		{"single", []time.Duration{150 * time.Millisecond}, 150 * time.Millisecond, 150 * time.Millisecond, 150 * time.Millisecond},
		{"three values", []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 300 * time.Millisecond}, 100 * time.Millisecond, 200 * time.Millisecond, 300 * time.Millisecond},
		{"unordered", []time.Duration{300 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond}, 100 * time.Millisecond, 200 * time.Millisecond, 300 * time.Millisecond},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			min, avg, max := summarize(tc.samples)
			if min != tc.min || avg != tc.avg || max != tc.max {
				t.Fatalf("summarize(%v) = min %v avg %v max %v, want min %v avg %v max %v",
					tc.samples, min, avg, max, tc.min, tc.avg, tc.max)
			}
		})
	}
}

func TestParseResolvConf(t *testing.T) {
	contents := `# a comment
nameserver 192.168.1.1
nameserver 1.1.1.1 ; trailing comment
search example.local
options ndots:2
nameserver 192.168.1.1
; another comment

nameserver 8.8.8.8 # trailing hash comment
`
	got := parseResolvConf(contents)
	want := []string{"192.168.1.1", "1.1.1.1", "8.8.8.8"}
	if len(got) != len(want) {
		t.Fatalf("parseResolvConf = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("parseResolvConf = %v, want %v", got, want)
		}
	}
	if got := parseResolvConf("options ndots:2\n"); len(got) != 0 {
		t.Fatalf("expected no servers, got %v", got)
	}
}

func TestParseDefaultGateway(t *testing.T) {
	header := "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n"

	t.Run("little endian gateway", func(t *testing.T) {
		contents := header +
			"eth0\t0001A8C0\t00000000\t0001\t0\t0\t0\t00FFFFFF\t0\t0\t0\n" +
			"eth0\t00000000\t0101A8C0\t0003\t0\t0\t100\t00000000\t0\t0\t0\n"
		gw, ok := parseDefaultGateway(contents)
		if !ok || gw != "192.168.1.1" {
			t.Fatalf("parseDefaultGateway = %q, %v; want 192.168.1.1, true", gw, ok)
		}
	})

	t.Run("lowest metric wins", func(t *testing.T) {
		contents := header +
			"eth0\t00000000\t0101A8C0\t0003\t0\t0\t600\t00000000\t0\t0\t0\n" +
			"wlan0\t00000000\t0100A8C0\t0003\t0\t0\t100\t00000000\t0\t0\t0\n"
		gw, ok := parseDefaultGateway(contents)
		if !ok || gw != "192.168.0.1" {
			t.Fatalf("parseDefaultGateway = %q, %v; want 192.168.0.1, true", gw, ok)
		}
	})

	t.Run("no default route", func(t *testing.T) {
		contents := header +
			"eth0\t0001A8C0\t00000000\t0001\t0\t0\t0\t00FFFFFF\t0\t0\t0\n"
		if gw, ok := parseDefaultGateway(contents); ok || gw != "" {
			t.Fatalf("parseDefaultGateway = %q, %v; want empty, false", gw, ok)
		}
	})

	t.Run("zero gateway skipped", func(t *testing.T) {
		contents := header +
			"eth0\t00000000\t00000000\t0001\t0\t0\t100\t00000000\t0\t0\t0\n"
		if _, ok := parseDefaultGateway(contents); ok {
			t.Fatal("expected no gateway for a zero gateway field")
		}
	})
}

func TestMeasureHTTPWithTestServer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "hello network")
	}))
	defer server.Close()

	result := measureHTTP(context.Background(), server.URL, 5*time.Second)
	if result.Error != "" {
		t.Fatalf("measureHTTP returned error: %s", result.Error)
	}
	if result.Status != http.StatusOK {
		t.Fatalf("status = %d, want %d", result.Status, http.StatusOK)
	}
	if result.Bytes <= 0 {
		t.Fatalf("bytes = %d, want > 0", result.Bytes)
	}
	if result.TotalMs <= 0 {
		t.Fatalf("total_ms = %v, want > 0", result.TotalMs)
	}
	if result.TTFBMs <= 0 {
		t.Fatalf("time_to_first_byte_ms = %v, want > 0", result.TTFBMs)
	}
}

func TestMeasureLoopback(t *testing.T) {
	result := measureLoopback(10)
	if result.Error != "" {
		t.Fatalf("measureLoopback returned error: %s", result.Error)
	}
	if result.Rounds != 10 {
		t.Fatalf("rounds = %d, want 10", result.Rounds)
	}
	if result.AvgMs < 0 {
		t.Fatalf("avg_ms = %v, want >= 0", result.AvgMs)
	}
}

func TestVerdict(t *testing.T) {
	tests := []struct {
		name       string
		findings   findings
		wantSlow   bool
		wantPhrase string
	}{
		{
			name: "everything healthy",
			findings: findings{
				DNSAvg: 20 * time.Millisecond, TCPAvg: 30 * time.Millisecond,
				HasHTTP: true, TTFB: 100 * time.Millisecond,
				HasLoopback: true, LoopbackAvg: time.Millisecond,
			},
			wantSlow:   false,
			wantPhrase: "No local cause found",
		},
		{
			name: "slow DNS",
			findings: findings{
				DNSAvg: 450 * time.Millisecond, TCPAvg: 40 * time.Millisecond,
				HasHTTP: true, TTFB: 100 * time.Millisecond,
				HasLoopback: true, LoopbackAvg: time.Millisecond,
			},
			wantSlow:   true,
			wantPhrase: "lookups are the bottleneck",
		},
		{
			name: "slow TCP with fast DNS",
			findings: findings{
				DNSAvg: 20 * time.Millisecond, TCPAvg: 900 * time.Millisecond,
				HasHTTP: true, TTFB: 100 * time.Millisecond,
				HasLoopback: true, LoopbackAvg: time.Millisecond,
			},
			wantSlow:   true,
			wantPhrase: "the path or the link itself",
		},
		{
			name: "fast connect but slow first byte",
			findings: findings{
				DNSAvg: 20 * time.Millisecond, TCPAvg: 40 * time.Millisecond,
				HasHTTP: true, TTFB: 2 * time.Second,
				HasLoopback: true, LoopbackAvg: time.Millisecond,
			},
			wantSlow:   true,
			wantPhrase: "the far end is slow to respond",
		},
		{
			name: "slow loopback means this device",
			findings: findings{
				DNSAvg: 20 * time.Millisecond, TCPAvg: 40 * time.Millisecond,
				HasHTTP: true, TTFB: 100 * time.Millisecond,
				HasLoopback: true, LoopbackAvg: 30 * time.Millisecond,
			},
			wantSlow:   true,
			wantPhrase: "the device itself is under load",
		},
		{
			name: "DNS failures",
			findings: findings{
				DNSFailed: true,
				HasHTTP:   true, HasLoopback: true, LoopbackAvg: time.Millisecond,
			},
			wantSlow:   true,
			wantPhrase: "DNS lookups failed outright",
		},
		{
			name: "slow download",
			findings: findings{
				DNSAvg: 20 * time.Millisecond, TCPAvg: 40 * time.Millisecond,
				HasHTTP: true, TTFB: 100 * time.Millisecond,
				HasLoopback: true, LoopbackAvg: time.Millisecond,
				HasDownload: true, DownloadMBps: 0.4,
			},
			wantSlow:   true,
			wantPhrase: "download speed is under 1 MB/s",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lines, slow := verdict(tc.findings)
			if slow != tc.wantSlow {
				t.Fatalf("verdict slow = %v, want %v (lines: %v)", slow, tc.wantSlow, lines)
			}
			joined := strings.Join(lines, "\n")
			if !strings.Contains(joined, tc.wantPhrase) {
				t.Fatalf("verdict lines %q do not contain %q", joined, tc.wantPhrase)
			}
		})
	}
}
