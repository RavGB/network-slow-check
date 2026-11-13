// Command network-slow-check finds out why a network feels slow,
// working only from the local device.
//
// RavGB - hello@ravgb.com - https://ravgb.com
//
// The major check groups run in parallel goroutines. Inside the DNS
// and TCP groups, each host is measured by its own goroutine, so a
// slow host never delays the measurement of the others. Results are
// written to per-group and per-host slots and read only after every
// goroutine has finished, which keeps the output order stable.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Exit codes.
const (
	exitOK    = 0 // no failing or slow findings
	exitSlow  = 1 // at least one slow or failed finding
	exitUsage = 2 // setup or usage error
)

type config struct {
	Hosts    []string
	Rounds   int
	Timeout  time.Duration
	URL      string
	Download string
	Output   string
	JSON     bool
}

type interfaceInfo struct {
	Name      string   `json:"name"`
	Up        bool     `json:"up"`
	Loopback  bool     `json:"loopback"`
	MTU       int      `json:"mtu"`
	Addresses []string `json:"addresses"`
}

// hostTiming is the reported summary of repeated measurements of one
// host. Times are milliseconds.
type hostTiming struct {
	Host      string  `json:"host"`
	Rounds    int     `json:"rounds"`
	Succeeded int     `json:"succeeded"`
	Errors    int     `json:"errors"`
	LastError string  `json:"last_error,omitempty"`
	MinMs     float64 `json:"min_ms"`
	AvgMs     float64 `json:"avg_ms"`
	MaxMs     float64 `json:"max_ms"`
}

type httpResult struct {
	URL     string  `json:"url"`
	Status  int     `json:"status,omitempty"`
	Error   string  `json:"error,omitempty"`
	DNSMs   float64 `json:"dns_ms"`
	TCPMs   float64 `json:"tcp_ms"`
	TLSMs   float64 `json:"tls_ms"`
	TTFBMs  float64 `json:"time_to_first_byte_ms"`
	TotalMs float64 `json:"total_ms"`
	Bytes   int64   `json:"bytes"`
}

type downloadResult struct {
	URL     string  `json:"url"`
	Error   string  `json:"error,omitempty"`
	Bytes   int64   `json:"bytes"`
	Seconds float64 `json:"seconds"`
	MBps    float64 `json:"mb_per_second"`
	Capped  bool    `json:"capped"`
}

type loopbackResult struct {
	Rounds int     `json:"rounds"`
	Error  string  `json:"error,omitempty"`
	AvgMs  float64 `json:"avg_ms"`
}

type report struct {
	Started     string          `json:"started"`
	Interfaces  []interfaceInfo `json:"interfaces"`
	Gateway     string          `json:"gateway,omitempty"`
	GatewayNote string          `json:"gateway_note,omitempty"`
	DNSServers  []string        `json:"dns_servers"`
	DNS         []hostTiming    `json:"dns_lookup_timing"`
	TCP         []hostTiming    `json:"tcp_connect_timing"`
	HTTP        httpResult      `json:"http_timing"`
	Download    *downloadResult `json:"download,omitempty"`
	Loopback    loopbackResult  `json:"loopback"`
	Verdict     []string        `json:"verdict"`
	Slow        bool            `json:"slow_or_failed_findings"`
}

func main() {
	os.Exit(run())
}

func run() int {
	cfg, err := parseFlags(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "network-slow-check:", err)
		return exitUsage
	}

	rep := collect(cfg)

	findings := buildFindings(rep)
	lines, slow := verdict(findings)
	rep.Verdict = lines
	rep.Slow = slow

	data, err := render(rep, cfg.JSON)
	if err != nil {
		fmt.Fprintln(os.Stderr, "network-slow-check: could not render report:", err)
		return exitUsage
	}
	if cfg.Output != "" {
		if err := os.WriteFile(cfg.Output, data, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "network-slow-check: could not save report:", err)
			return exitUsage
		}
		fmt.Printf("Report saved to %s\n", cfg.Output)
	} else {
		fmt.Print(string(data))
	}

	if slow {
		return exitSlow
	}
	return exitOK
}

func parseFlags(args []string) (config, error) {
	var cfg config
	var hosts string
	fs := flag.NewFlagSet("network-slow-check", flag.ContinueOnError)
	fs.StringVar(&hosts, "hosts", "example.com,github.com,ravgb.com",
		"comma-separated hosts for the DNS and TCP timing checks")
	fs.IntVar(&cfg.Rounds, "rounds", 3, "rounds of DNS and TCP timing per host")
	fs.DurationVar(&cfg.Timeout, "timeout", 5*time.Second, "timeout per single operation")
	fs.StringVar(&cfg.URL, "url", "https://example.com/", "URL for the HTTP timing check")
	fs.StringVar(&cfg.Download, "download", "",
		"optional URL for a throughput test (byte cap and time limit apply)")
	fs.StringVar(&cfg.Output, "output", "", "also save the report to this file")
	fs.BoolVar(&cfg.JSON, "json", false,
		"write the report as JSON instead of text (to standard output, or to the --output file when one is set)")
	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	if fs.NArg() > 0 {
		return cfg, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	for _, h := range strings.Split(hosts, ",") {
		if h = strings.TrimSpace(h); h != "" {
			cfg.Hosts = append(cfg.Hosts, h)
		}
	}
	if len(cfg.Hosts) == 0 {
		return cfg, fmt.Errorf("at least one host is required")
	}
	if cfg.Rounds < 1 {
		return cfg, fmt.Errorf("--rounds must be at least 1")
	}
	if cfg.Timeout <= 0 {
		return cfg, fmt.Errorf("--timeout must be greater than zero")
	}
	if err := checkURL(cfg.URL); err != nil {
		return cfg, fmt.Errorf("--url: %w", err)
	}
	if cfg.Download != "" {
		if err := checkURL(cfg.Download); err != nil {
			return cfg, fmt.Errorf("--download: %w", err)
		}
	}
	return cfg, nil
}

func checkURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme must be http or https")
	}
	if u.Host == "" {
		return fmt.Errorf("host is missing")
	}
	return nil
}

// collect runs the five check groups in parallel goroutines and
// gathers their results once all of them have finished.
func collect(cfg config) report {
	rep := report{Started: time.Now().Format("2006-01-02 15:04:05")}

	var wg sync.WaitGroup

	// Group 1: local setup - interfaces, default gateway, DNS servers.
	wg.Add(1)
	go func() {
		defer wg.Done()
		rep.Interfaces = listInterfaces()
		gw, note := defaultGateway()
		rep.Gateway = gw
		rep.GatewayNote = note
		rep.DNSServers = dnsServers()
	}()

	// Group 2: DNS lookup timing, one goroutine per host inside.
	wg.Add(1)
	go func() {
		defer wg.Done()
		rep.DNS = measureAllHosts(cfg, measureDNSRound)
	}()

	// Group 3: TCP connect timing, one goroutine per host inside.
	wg.Add(1)
	go func() {
		defer wg.Done()
		rep.TCP = measureAllHosts(cfg, measureTCPRound)
	}()

	// Group 4: HTTP timing, plus the optional throughput test.
	wg.Add(1)
	go func() {
		defer wg.Done()
		rep.HTTP = measureHTTP(context.Background(), cfg.URL, cfg.Timeout)
		if cfg.Download != "" {
			dl := measureDownload(cfg.Download)
			rep.Download = &dl
		}
	}()

	// Group 5: loopback round trips on this device itself.
	wg.Add(1)
	go func() {
		defer wg.Done()
		rep.Loopback = measureLoopback(loopbackRounds)
	}()

	wg.Wait()
	return rep
}

// buildFindings reduces a report to the numbers the verdict rules use.
// Averages are the worst (largest) per-host average, so one bad host is
// enough to surface a finding.
func buildFindings(rep report) findings {
	var f findings

	for _, h := range rep.DNS {
		if h.Errors > 0 {
			f.DNSFailed = true
		}
		if h.Succeeded > 0 && msToDuration(h.AvgMs) > f.DNSAvg {
			f.DNSAvg = msToDuration(h.AvgMs)
		}
	}
	for _, h := range rep.TCP {
		if h.Errors > 0 {
			f.TCPFailed = true
		}
		if h.Succeeded > 0 && msToDuration(h.AvgMs) > f.TCPAvg {
			f.TCPAvg = msToDuration(h.AvgMs)
		}
	}
	f.HasHTTP = true
	if rep.HTTP.Error != "" {
		f.HTTPFailed = true
	} else {
		f.TTFB = msToDuration(rep.HTTP.TTFBMs)
	}
	f.HasLoopback = true
	if rep.Loopback.Error != "" {
		f.LoopbackFailed = true
	} else {
		f.LoopbackAvg = msToDuration(rep.Loopback.AvgMs)
	}
	if rep.Download != nil {
		f.HasDownload = true
		if rep.Download.Error != "" {
			f.DownloadFailed = true
		} else {
			f.DownloadMBps = rep.Download.MBps
		}
	}
	return f
}

func msToDuration(ms float64) time.Duration {
	return time.Duration(ms * float64(time.Millisecond))
}

func durationMs(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}

// render produces the report bytes: JSON when requested, text
// otherwise. The bytes go to standard output, or to the --output file
// when one is set (standard output then carries only a short saved
// note, printed by the caller).
func render(rep report, asJSON bool) ([]byte, error) {
	if asJSON {
		data, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			return nil, err
		}
		return append(data, '\n'), nil
	}
	return []byte(buildTextReport(rep)), nil
}

func buildTextReport(rep report) string {
	var b strings.Builder
	b.WriteString("Network slow check\n")
	b.WriteString("RavGB - hello@ravgb.com - https://ravgb.com\n")
	fmt.Fprintf(&b, "Started: %s\n", rep.Started)
	b.WriteString(strings.Repeat("-", 64) + "\n")

	b.WriteString("Interfaces:\n")
	if len(rep.Interfaces) == 0 {
		b.WriteString("  (none found)\n")
	}
	for _, iface := range rep.Interfaces {
		state := "down"
		if iface.Up {
			state = "up"
		}
		kind := ""
		if iface.Loopback {
			kind = ", loopback"
		}
		fmt.Fprintf(&b, "  %s: %s%s, MTU %d, addresses %s\n",
			iface.Name, state, kind, iface.MTU, strings.Join(iface.Addresses, ", "))
	}
	if rep.Gateway != "" {
		fmt.Fprintf(&b, "Default gateway: %s\n", rep.Gateway)
	} else if rep.GatewayNote != "" {
		fmt.Fprintf(&b, "Default gateway: %s\n", rep.GatewayNote)
	}
	if len(rep.DNSServers) > 0 {
		fmt.Fprintf(&b, "DNS servers: %s\n", strings.Join(rep.DNSServers, ", "))
	} else {
		b.WriteString("DNS servers: (none found in resolv.conf)\n")
	}

	b.WriteString("\nDNS lookup timing:\n")
	for _, h := range rep.DNS {
		writeHostTiming(&b, h)
	}
	b.WriteString("\nTCP connect timing (port 443):\n")
	for _, h := range rep.TCP {
		writeHostTiming(&b, h)
	}

	h := rep.HTTP
	b.WriteString("\nHTTP timing:\n")
	if h.Error != "" {
		fmt.Fprintf(&b, "  %s: failed: %s\n", h.URL, h.Error)
	} else {
		fmt.Fprintf(&b, "  %s: status %d, %d bytes\n", h.URL, h.Status, h.Bytes)
		fmt.Fprintf(&b, "    DNS %.1f ms, TCP connect %.1f ms, TLS handshake %.1f ms\n",
			h.DNSMs, h.TCPMs, h.TLSMs)
		fmt.Fprintf(&b, "    First byte %.1f ms, total %.1f ms\n", h.TTFBMs, h.TotalMs)
	}

	if rep.Download != nil {
		d := rep.Download
		b.WriteString("\nDownload test:\n")
		if d.Error != "" {
			fmt.Fprintf(&b, "  %s: failed: %s\n", d.URL, d.Error)
		} else {
			capped := ""
			if d.Capped {
				capped = " (stopped at the byte cap)"
			}
			fmt.Fprintf(&b, "  %s: %d bytes in %.2f s = %.2f MB/s%s\n",
				d.URL, d.Bytes, d.Seconds, d.MBps, capped)
		}
	}

	b.WriteString("\nLoopback (this device itself):\n")
	if rep.Loopback.Error != "" {
		fmt.Fprintf(&b, "  failed: %s\n", rep.Loopback.Error)
	} else {
		fmt.Fprintf(&b, "  %d round trips, average %.3f ms\n", rep.Loopback.Rounds, rep.Loopback.AvgMs)
	}

	b.WriteString("\nVerdict:\n")
	for _, line := range rep.Verdict {
		fmt.Fprintf(&b, "  - %s\n", line)
	}
	return b.String()
}

func writeHostTiming(b *strings.Builder, h hostTiming) {
	if h.Succeeded == 0 {
		fmt.Fprintf(b, "  %s: all %d round(s) failed, last error: %s\n", h.Host, h.Rounds, h.LastError)
		return
	}
	extra := ""
	if h.Errors > 0 {
		extra = fmt.Sprintf(", %d error(s)", h.Errors)
	}
	fmt.Fprintf(b, "  %s: min %.1f ms, avg %.1f ms, max %.1f ms%s\n",
		h.Host, h.MinMs, h.AvgMs, h.MaxMs, extra)
}
