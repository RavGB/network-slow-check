package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync"
	"time"
)

// summarize reduces samples to min, average, and max. An empty slice
// gives zeroes. Pure function, unit tested.
func summarize(samples []time.Duration) (min, avg, max time.Duration) {
	if len(samples) == 0 {
		return 0, 0, 0
	}
	min, max = samples[0], samples[0]
	var total time.Duration
	for _, s := range samples {
		if s < min {
			min = s
		}
		if s > max {
			max = s
		}
		total += s
	}
	return min, total / time.Duration(len(samples)), max
}

// roundFunc measures one round against one host.
type roundFunc func(ctx context.Context, host string, timeout time.Duration) error

// measureAllHosts measures every host in its own goroutine. Each
// goroutine writes only to its own slot of the results slice, and the
// slice is read after all goroutines finish, so the report order
// always matches the order the hosts were given in.
func measureAllHosts(cfg config, round roundFunc) []hostTiming {
	results := make([]hostTiming, len(cfg.Hosts))
	var wg sync.WaitGroup
	for i, host := range cfg.Hosts {
		wg.Add(1)
		go func(i int, host string) {
			defer wg.Done()
			results[i] = measureHost(host, cfg.Rounds, cfg.Timeout, round)
		}(i, host)
	}
	wg.Wait()
	return results
}

// measureHost runs the rounds for one host one after another, so the
// rounds of a single host never inflate each other.
func measureHost(host string, rounds int, timeout time.Duration, round roundFunc) hostTiming {
	timing := hostTiming{Host: host, Rounds: rounds}
	var samples []time.Duration
	for r := 0; r < rounds; r++ {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		start := time.Now()
		err := round(ctx, host, timeout)
		elapsed := time.Since(start)
		cancel()
		if err != nil {
			timing.Errors++
			timing.LastError = err.Error()
			continue
		}
		samples = append(samples, elapsed)
	}
	timing.Succeeded = len(samples)
	min, avg, max := summarize(samples)
	timing.MinMs = durationMs(min)
	timing.AvgMs = durationMs(avg)
	timing.MaxMs = durationMs(max)
	return timing
}

func measureDNSRound(ctx context.Context, host string, _ time.Duration) error {
	resolver := &net.Resolver{}
	_, err := resolver.LookupHost(ctx, host)
	return err
}

func measureTCPRound(ctx context.Context, host string, timeout time.Duration) error {
	dialer := &net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, "443"))
	if err != nil {
		return err
	}
	return conn.Close()
}

// measureHTTP performs one GET and breaks its time down with
// httptrace: DNS lookup, TCP connect, TLS handshake, time to first
// byte, and total time including reading the body. A phase that does
// not happen (for example DNS when the address is already known, or
// TLS on a plain http URL) is reported as zero.
func measureHTTP(ctx context.Context, rawURL string, timeout time.Duration) httpResult {
	result := httpResult{URL: rawURL}

	var dnsStart, connectStart, tlsStart, firstByte time.Time
	var dnsTime, connectTime, tlsTime time.Duration

	trace := &httptrace.ClientTrace{
		DNSStart: func(_ httptrace.DNSStartInfo) { dnsStart = time.Now() },
		DNSDone: func(_ httptrace.DNSDoneInfo) {
			if !dnsStart.IsZero() {
				dnsTime = time.Since(dnsStart)
			}
		},
		ConnectStart: func(_, _ string) { connectStart = time.Now() },
		ConnectDone: func(_, _ string, _ error) {
			if !connectStart.IsZero() {
				connectTime = time.Since(connectStart)
			}
		},
		TLSHandshakeStart: func() { tlsStart = time.Now() },
		TLSHandshakeDone: func(_ tls.ConnectionState, _ error) {
			if !tlsStart.IsZero() {
				tlsTime = time.Since(tlsStart)
			}
		},
		GotFirstResponseByte: func() { firstByte = time.Now() },
	}

	reqCtx, cancel := context.WithTimeout(ctx, timeout*4)
	defer cancel()
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(reqCtx, trace), http.MethodGet, rawURL, nil)
	if err != nil {
		result.Error = err.Error()
		return result
	}

	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		result.Error = err.Error()
		result.TotalMs = durationMs(time.Since(start))
		return result
	}
	defer resp.Body.Close()

	result.Status = resp.StatusCode
	body, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
	total := time.Since(start)
	if err != nil {
		result.Error = fmt.Sprintf("reading body: %v", err)
		result.TotalMs = durationMs(total)
		return result
	}
	result.Bytes = int64(len(body))
	if !firstByte.IsZero() {
		result.TTFBMs = durationMs(firstByte.Sub(start))
	}
	result.DNSMs = durationMs(dnsTime)
	result.TCPMs = durationMs(connectTime)
	result.TLSMs = durationMs(tlsTime)
	result.TotalMs = durationMs(total)
	return result
}

const (
	// downloadMaxBytes caps the throughput test so a huge file cannot
	// turn a quick check into a long download.
	downloadMaxBytes = 10 << 20 // 10 MiB
	// downloadTimeout bounds the whole throughput test.
	downloadTimeout = 30 * time.Second
)

// measureDownload downloads up to downloadMaxBytes from rawURL and
// reports the achieved throughput.
func measureDownload(rawURL string) downloadResult {
	result := downloadResult{URL: rawURL}
	ctx, cancel := context.WithTimeout(context.Background(), downloadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	defer resp.Body.Close()
	written, err := io.Copy(io.Discard, io.LimitReader(resp.Body, downloadMaxBytes+1))
	seconds := time.Since(start).Seconds()
	if err != nil {
		result.Error = err.Error()
		return result
	}
	if written > downloadMaxBytes {
		written = downloadMaxBytes
		result.Capped = true
	}
	result.Bytes = written
	result.Seconds = seconds
	if seconds > 0 {
		result.MBps = float64(written) / seconds / (1024 * 1024)
	}
	return result
}

const loopbackRounds = 10

// measureLoopback opens a listener on the loopback interface and does
// the requested number of request/response round trips over it. A
// slow result here points at this device, not at the network.
func measureLoopback(rounds int) loopbackResult {
	result := loopbackResult{Rounds: rounds}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		result.Error = err.Error()
		return result
	}
	defer listener.Close()

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go echoConn(conn)
		}
	}()

	var samples []time.Duration
	for i := 0; i < rounds; i++ {
		if err := loopbackOnce(listener.Addr().String(), &samples); err != nil {
			result.Error = err.Error()
			return result
		}
	}
	_, avg, _ := summarize(samples)
	result.AvgMs = durationMs(avg)
	return result
}

func echoConn(conn net.Conn) {
	defer conn.Close()
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err == nil {
		_, _ = conn.Write(buf)
	}
}

func loopbackOnce(address string, samples *[]time.Duration) error {
	start := time.Now()
	conn, err := net.DialTimeout("tcp", address, 2*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return err
	}
	if _, err := conn.Write([]byte("ping")); err != nil {
		return err
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil {
		return err
	}
	*samples = append(*samples, time.Since(start))
	return nil
}
