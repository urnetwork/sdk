# lagdev

lag.dev's latency and bufferbloat measurements, from a terminal and as an MCP
server for coding and desktop agents. It runs on your machine, so it measures
the network your machine is on. A remote server would measure its own.

It is built on the SDK's lag client (`../src/lag`), the code lag.dev runs in
the browser. It measures against the same servers with the same guest sessions,
tickets and limits. lag.dev/PLAN.md amendment 12, item 17.

## Run it

It needs Node 22 or later.

```sh
cd sdk/js/lag-cli
npm install
npm run build
node dist/cli.js                # latency: 15 s of pings, no load
node dist/cli.js bufferbloat    # loads each server in turn, within 500 MB and 180 s
```

`npm link` puts `lagdev` on your PATH. The package isn't published to npm.

```
lagdev [latency] [options]      ping up to 8 servers with no load (default 15 s)
lagdev bufferbloat [options]    then load each in turn: a 5 s download, a 5 s upload
lagdev mcp [options]            serve measure_latency and measure_bufferbloat over MCP stdio

  --json                 print the result as JSON
  --duration <s>         latency: how long to ping (default 15)
  --servers <n>          how many servers, 1 to 8 (default 8)
  --max-bytes <size>     bufferbloat: stop before moving more, e.g. 500MB, 2GB (default 500MB)
  --max-seconds <s>      bufferbloat: stop before running longer (default 180)
  --api <url>            the api (default https://api.bringyour.com)
  --stack <stack.json>   a local lag.dev test stack (lag.dev/e2e/stack): its api, front and CA
  --ca <file>            also trust this root, PEM
  --connect-to <h:p>     send every connection to this address, keeping TLS names
  --origin <origin>      the page origin measurement requests carry (default https://lag.dev)
  --no-save              don't keep the guest session between runs
  --config-dir <dir>     where the guest session is kept (default ~/.config/lagdev)
  -q, --quiet            no progress on stderr
```

The result goes to stdout and progress to stderr. Ctrl-C stops a run and
closes its sessions; a second Ctrl-C exits at once.

The exit code is 0 for a result, 2 for bad arguments, and 1 when the
measurement couldn't run. With `--json`, a failure prints
`{"error", "message", "retryAfterMs"?}` to stdout. The api refuses a new guest
or a track for a while when a network asks for too many, and `retryAfterMs`
says how long to wait.

```
$ lagdev
lag.dev latency · from Austin, Texas, US · 8 servers · 16 s

  server                     km  p50 ms  p95 ms  lost  pings  RPM
  Houston, US               235    13.1    16.8     0     62  4,580 good
  Dallas, US                293    21.1    26.3     0     62  2,844 good
  Mexico City, MX         1,213    29.6    34.9     0     62  2,027 good
  Toronto, CA             2,199    41.5    59.2     0     61  1,446 good
  London, GB              7,910   121.6   127.8     0     60  493 fair
  São Paulo, BR           8,095   146.1   165.6     0     60  411 fair
  Frankfurt am Main, DE   8,528   133.2   138.9     0     60  450 fair
  Tokyo, JP              10,544   170.1   179.5     0     60  353 fair

  best: Houston at 13.1 ms · idle RPM 4,580 good
  all servers: p50 68.0 ms · p95 171.2 ms · 0 of 487 pings lost
```

## What it measures

**Servers.** It uses up to 8 lag.dev servers, picked as lag.dev picks them:
- the 3 nearest, in different cities;
- then far ones, at least 1,500 km from you and 500 km from the near ones, in
  new countries first.

Four spares stand in for any server whose session won't open. Your location
comes from the api's `my-ip-info`, as a city and a network (ASN).

**Latency** (`lagdev`, `measure_latency`). Each server sends this machine a
WebSocket ping about 4 times a second and times the pong itself. Nothing loads
the line. For each server and overall, it reports:
- the idle round trip, p50 and p95 in ms (nearest-rank percentiles, as on
  lag.dev);
- lost pings, those with no pong within 5 s. The pings ride TCP, so a dropped
  packet shows up as delay, not as a loss. This is not a packet-loss
  measurement.
- RPM, or round trips per minute: 60,000 divided by the p50 in ms. It is banded
  as in draft-ietf-ippm-responsiveness-09: poor below 300, fair to 1,000, good
  to 6,000, excellent from 6,000.

The best server is the one with the lowest idle p50. The overall RPM is that
server's, so it reflects your access network without the distance to far
servers. Idle RPM is a best case. The draft measures RPM under working
conditions, which is closer to bufferbloat's loaded RPM.

**Bufferbloat** (`lagdev bufferbloat`, `measure_bufferbloat`). It pings every
server for 5 s with no load, then loads each server in turn: a 5 s download,
then a 5 s upload, over 4 streams. Every server keeps pinging throughout. Only
one load runs at a time, because a server admits one load at a time from a
network.

For each server, it reports:
- download and upload p50 and p95 in Mbps, over 250 ms intervals. The upload
  counts only what the server says it received (`verified`).
- the loaded increase for each direction: the round trip's p50 during that
  direction, minus the idle p50. A ping counts as idle only when its whole
  round trip falls outside every load and the 2 s after it.
- loaded RPM, from the round trips during that server's own load.

Overall, it reports the medians of the increases and of loaded RPM over the
servers, plus the best and median speeds.

## Data cost

- **Latency** uses well under 1 MB.
- **Bufferbloat** moves real data. Each server's load is 5 s of download and
  5 s of upload at your line's speed. That comes to about 5 MB per Mbps of your
  combined download and upload speed for all 8 servers. A 100/20 Mbps line uses
  about 600 MB, and a gigabit line would use more than 5 GB without a cap.

`--max-bytes` (default 500 MB) and `--max-seconds` (default 180) cap a
bufferbloat run, and it stops early at either one:
- Before each load, lagdev predicts the load's size from the largest load so
  far, and doesn't start a load that won't fit.
- During a load, it counts bytes as they move and cuts the load at the cap.
  On a fast line, that is within a few MB.
- `stoppedEarly` names the cap that stopped the run, and servers it didn't load
  are `skipped`.

The cap counts every byte sent. The result's `bytes` counts what was measured:
downloads as read, uploads as verified. The MCP tool has no defaults for
either cap, so an agent has to choose both. On a metered line, use latency.

## Privacy

Nothing about this machine's address is stored, the same as on lag.dev:
- The api's `my-ip-info` tells lagdev where this machine is. lagdev keeps the
  city, region, country and network (ASN) for its output, and never the
  address.
- The measurements the servers keep have the city and network, not the
  address.

The guest session is kept between runs, so a run doesn't start a new one. (The
api allows a network 10 new guests an hour.) It is a jwt, stored by api in
`~/.config/lagdev/guest-<api>.json`. The directory is `$XDG_CONFIG_HOME/lagdev`
when that is set, or `%APPDATA%\lagdev` on Windows. The file is readable only
by you (0600). `--no-save` keeps the session in memory instead.

Measurement requests to the servers carry `Origin: https://lag.dev`, because
the servers refuse a request from an origin they don't serve, and lagdev
measures as lag.dev. The api's requests carry no Origin.

## MCP

The server speaks MCP over stdio. To add it to Claude Code:

```sh
claude mcp add lagdev -- node /path/to/sdk/js/lag-cli/dist/cli.js mcp
```

Or in a client's JSON config:

```json
{
  "mcpServers": {
    "lagdev": { "command": "node", "args": ["/path/to/sdk/js/lag-cli/dist/cli.js", "mcp"] }
  }
}
```

| tool | arguments | cost |
| --- | --- | --- |
| `measure_latency` | `duration_seconds` 3–60 (default 15), `servers` 1–8 (default 8) | well under 1 MB |
| `measure_bufferbloat` | `max_bytes` 1,000,000–20,000,000,000 and `max_seconds` 20–600, both required; `servers` 1–8 (default 8) | about 5 MB per Mbps of the line, capped |

Both tools return the CLI's `--json` result as text. A failure comes back with
`isError` and `{"error", "message", "retryAfterMs"?}`. One measurement runs at
a time: two at once would load the same line and spoil each other, so a second
call waits for the first. `lagdev mcp` takes the CLI's connection options, so
`lagdev mcp --stack …` serves measurements of a test stack.

## A test stack

lag.dev's e2e stack (`mmm/lag.dev/e2e/stack`) serves the production names
behind one front, with its own CA. `--stack` reads its `stack.json` for the api,
the front's address and the CA:

```sh
node dist/cli.js --stack ../../../mmm/lag.dev/e2e/stack/stack.json --config-dir /tmp/lagdev-stack
```

That is shorthand for
`--api https://api.bringyour.com --connect-to 127.0.0.1:47443 --ca <stack>/.state/front/ca/roots.pem`.
Every connection, to the api and to each `<id>.extender.bringyour.com`, goes to
the front, and TLS keeps the real name. `--api https://localhost:47443` alone
won't work. The front routes by name, and the tickets name the extenders by
their production hosts.

## Tests

```sh
npm test            # node --test, against the SDK's mock extender (../test/lag/client_mock_extender.ts)
npm run typecheck
```

The tests run a mock api with a mock extender per city, all on 127.0.0.1. They
check:
- server selection;
- latency and bufferbloat runs, and both caps;
- guest reuse and replacement, and the api's refusals;
- spares;
- the Origin header;
- the CLI's output and exit codes;
- the MCP tools.
