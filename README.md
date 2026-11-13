# Network Slow Check

A small tool from RavGB that finds out **why a network feels slow**,
working only from the local device. It does not just say "slow" - it
measures each stage separately and ends with a plain verdict about
the likely cause.

Contact: hello@ravgb.com
Site: https://ravgb.com

It is written in Go using only the standard library, and it builds
to a single binary with nothing to install on the machine it checks.
The check groups run in parallel goroutines, and inside the timing
groups each host is measured by its own goroutine, so one slow host
never holds up the rest. Reports still print in a stable order.

## What it checks

- **Interfaces** - every network interface, up or down, loopback or
  not, with its addresses and MTU.
- **Default gateway** - read from `/proc/net/route` on Linux (the
  lowest-metric default route wins). Skipped with a note on other
  systems.
- **DNS servers** - the `nameserver` lines from `/etc/resolv.conf`.
- **DNS lookup timing** - repeated lookups for each host, reported
  as min / average / max, with errors counted.
- **TCP connect timing** - repeated connections to port 443 for each
  host, reported the same way. Failures are reported, not hidden.
- **HTTP timing** - one full request broken down by stage: DNS,
  TCP connect, TLS handshake, time to first byte, total time,
  status code, and bytes read.
- **Download test (optional)** - with `--download URL`, downloads up
  to 10 MiB with a 30 second limit and reports bytes, seconds, and
  MB/s.
- **Loopback** - 10 round trips over a socket on this device itself.
  This separates "this device is slow" from "the network is slow".

## How to read the verdict

The last section is the point of the tool. Thresholds: DNS average
over 200 ms, TCP connect average over 300 ms, first byte over
800 ms, loopback average over 5 ms, download under 1 MB/s.

- **DNS slow, everything else fast** - lookups are the bottleneck.
  Every new connection waits on a slow name lookup first. Try a
  different DNS server to compare.
- **DNS fast, TCP connect slow** - the path or the link itself:
  weak signal, congestion, or a long route.
- **Connect fast, first byte slow** - the far end is slow to
  respond. The local network looks fine.
- **Loopback slow** - this device itself is under load. Close heavy
  programs and run the check again before blaming the network.
- **Failures** - failed lookups or connections are named as
  failures, with the likely first place to look.
- **Nothing slow** - "No local cause found": DNS, connections,
  first response, and this device all look healthy.

## Build

```bash
go build -o network-slow-check .
```

## Run

```bash
./network-slow-check
```

Options:

```bash
./network-slow-check --hosts ravgb.com,github.com --rounds 5
./network-slow-check --url https://ravgb.com/ --timeout 3s
./network-slow-check --download https://example.com/some-file.bin
./network-slow-check --output report.txt
./network-slow-check --json --output report.json
```

- `--hosts` - comma-separated hosts for the DNS and TCP checks
  (default `example.com,github.com,ravgb.com`).
- `--rounds` - rounds per host (default 3).
- `--timeout` - timeout for a single operation (default 5s).
- `--url` - URL for the HTTP timing check
  (default `https://example.com/`).
- `--download` - optional URL for the throughput test.
- `--output` - also save the report to this file. When a file is
  set, the report goes to the file and the screen shows only a
  short saved note.
- `--json` - machine-readable report. It goes to the screen, or to
  the `--output` file when one is set.

Exit codes: **0** means nothing slow and nothing failed, **1**
means at least one slow or failed finding, **2** means a setup or
usage error (bad flags, an unreadable report path, and the like).

## Example output

```text
Network slow check
RavGB - hello@ravgb.com - https://ravgb.com
Started: 2026-11-13 11:45:00
----------------------------------------------------------------
Interfaces:
  lo: up, loopback, MTU 65536, addresses 127.0.0.1/8, ::1/128
  eth0: up, MTU 1500, addresses 192.168.1.50/24
Default gateway: 192.168.1.1
DNS servers: 192.168.1.1

DNS lookup timing:
  example.com: min 12.0 ms, avg 18.0 ms, max 25.0 ms
  github.com: min 15.0 ms, avg 22.0 ms, max 31.0 ms
  ravgb.com: min 11.0 ms, avg 16.0 ms, max 20.0 ms

TCP connect timing (port 443):
  example.com: min 20.0 ms, avg 24.0 ms, max 29.0 ms
  github.com: min 35.0 ms, avg 41.0 ms, max 48.0 ms
  ravgb.com: min 28.0 ms, avg 33.0 ms, max 40.0 ms

HTTP timing:
  https://example.com/: status 200, 559 bytes
    DNS 12.0 ms, TCP connect 20.0 ms, TLS handshake 30.0 ms
    First byte 55.0 ms, total 90.0 ms

Loopback (this device itself):
  10 round trips, average 0.050 ms

Verdict:
  - Nothing measured as slow and nothing failed. No local cause found: DNS, connections, first response, and this device all look healthy.
```

## Tests

```bash
go test ./...
go test -race ./...
```

The tests cover the latency statistics, the resolv.conf parser, the
gateway parser (fed sample route tables, including the little-endian
gateway address and the lowest-metric rule), the verdict rules as a
table, an HTTP timing run against a local test server, and the
loopback measurement.

## License

MIT License. See the LICENSE file in this repository.

Copyright (c) 2026 RavGB Technologies Inc.
