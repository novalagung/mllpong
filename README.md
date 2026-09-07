# MLLPong - HL7 MLLP Mock Server

<p align="center">
  <img src="https://cdn.novalagung.com/mllpong/images/mllpong.png" alt="MLLPong" width="180">
</p>

<p align="center">
  <a href="https://github.com/novalagung/mllpong/releases/latest"><img src="https://img.shields.io/github/v/release/novalagung/mllpong"></a>
  <a href="#"><img src="https://img.shields.io/github/actions/workflow/status/novalagung/mllpong/docker-image.yml"></a>
  <a href="#"><img src="https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/novalagung/mllpong/master/coverage.json"></a>
  <a href="https://hub.docker.com/r/novalagung/mllpong"><img src="https://img.shields.io/docker/pulls/novalagung/mllpong"></a>
  <a href="#"><img src="https://img.shields.io/github/downloads/novalagung/mllpong/total?label=binary%20installs"></a>
</p>

MLLPong is an open-source mock server for testing HL7 v2 messages over MLLP (Minimal Lower Layer Protocol) — packed into a single binary under **1 MB**. No runtime, no dependencies, no bloat.

It exposes three TCP endpoints with different behaviors:

| Endpoint | Default Port | Behavior |
| --- | --- | --- |
| ACK handler | `2575` | Always replies with `AA` (application accept) |
| Chaos handler | `2576` | Always replies with `AR` (application reject) |
| Smart handler | `2577` | Responds based on message type rules from a JSON config file |

Every message received on any of the three ports can optionally be written to disk for later inspection. See [Saving Inbound Messages](#saving-inbound-messages).

## Installation

#### ◉ Homebrew (macOS / Linux)

```bash
brew install novalagung/tap/mllpong
```

#### ◉ Binary (macOS / Linux)

```bash
curl -sSfL https://raw.githubusercontent.com/novalagung/mllpong/master/install.sh | sh
```

#### ◉ Linux packages

Download the `.deb`, `.rpm`, or `.apk` from the [latest release](https://github.com/novalagung/mllpong/releases/latest), then install:

```bash
# Debian / Ubuntu
sudo dpkg -i mllpong_linux_amd64.deb

# RHEL / Fedora
sudo rpm -i mllpong_linux_amd64.rpm

# Alpine
sudo apk add --allow-untrusted mllpong_linux_amd64.apk
```

#### ◉ Docker

Available on both Docker Hub and GHCR:

```bash
# Docker Hub
docker pull novalagung/mllpong:latest

# GHCR
docker pull ghcr.io/novalagung/mllpong:latest
```

## Running MLLPong

#### ◉ Binary

Once installed, start the server with defaults (ACK on `2575`, Chaos on `2576`, Smart on `2577`):

```bash
mllpong
```

With CLI flags:

```bash
mllpong --ack-port 2575 --chaos-port 2576 --smart-port 2577 --rules-file /etc/hl7/rules.json --save-path ./files
```

To see all available options:

```bash
mllpong --help
```

#### ◉ Docker

```bash
docker run -d \
  -e ACK_PORT=2575 \
  -e CHAOS_PORT=2576 \
  -e SMART_PORT=2577 \
  -p 2575:2575 \
  -p 2576:2576 \
  -p 2577:2577 \
  novalagung/mllpong:latest
```

Or with custom `rules.json`:

```bash
docker run -d \
  -e ACK_PORT=2575 \
  -e CHAOS_PORT=2576 \
  -e SMART_PORT=2577 \
  -e RULES_FILE=/etc/hl7/rules.json \
  -p 2575:2575 \
  -p 2576:2576 \
  -p 2577:2577 \
  -v ./rules.json:/etc/hl7/rules.json:ro \
  novalagung/mllpong:latest
```

Or with inbound messages saved to `./files`:

```bash
docker run -d \
  -e ACK_PORT=2575 \
  -e CHAOS_PORT=2576 \
  -e SMART_PORT=2577 \
  -e RULES_FILE=/etc/hl7/rules.json \
  -e SAVE_PATH=/var/hl7/files \
  -p 2575:2575 \
  -p 2576:2576 \
  -p 2577:2577 \
  -v ./rules.json:/etc/hl7/rules.json:ro \
  -v ./files:/var/hl7/files \
  novalagung/mllpong:latest
```

#### ◉ Docker Compose

```bash
services:
  mllpong:
    image: novalagung/mllpong:latest
    # build: .
    environment:
      HOST: "0.0.0.0"
      ACK_PORT: 2575
      CHAOS_PORT: 2576
      SMART_PORT: 2577
    ports:
      - "2575:2575"
      - "2576:2576"
      - "2577:2577"
    restart: unless-stopped
```

Or with custom `rules.json`:

```bash
services:
  mllpong:
    image: novalagung/mllpong:latest
    # build: .
    environment:
      HOST: "0.0.0.0"
      ACK_PORT: 2575
      CHAOS_PORT: 2576
      SMART_PORT: 2577
      RULES_FILE: /etc/hl7/rules.json
    ports:
      - "2575:2575"
      - "2576:2576"
      - "2577:2577"
    volumes:
      - ./rules.json:/etc/hl7/rules.json:ro
    restart: unless-stopped
```

Or with inbound messages saved to `./files`:

```bash
services:
  mllpong:
    image: novalagung/mllpong:latest
    # build: .
    environment:
      HOST: "0.0.0.0"
      ACK_PORT: 2575
      CHAOS_PORT: 2576
      SMART_PORT: 2577
      RULES_FILE: /etc/hl7/rules.json
      SAVE_PATH: /var/hl7/files
    ports:
      - "2575:2575"
      - "2576:2576"
      - "2577:2577"
    volumes:
      - ./rules.json:/etc/hl7/rules.json:ro
      - ./files:/var/hl7/files
    restart: unless-stopped
```

## Testing the Server

Send any valid HL7 v2 message wrapped in MLLP framing to any port. A quick smoke test with `netcat`:

```bash
# ACK handler — expect MSA|AA
printf '\x0bMSH|^~\&|sender|sender|receiver|receiver|20240101120000||ADT^A01^ADT_A01|MSG001|P|2.5\rEVN||20240101120000\rPID|||123456||Doe^John\r\x1c\x0d' | nc localhost 2575 | tr '\r' '\n'

# Chaos handler — expect MSA|AR
printf '\x0bMSH|^~\&|sender|sender|receiver|receiver|20240101120000||ADT^A01^ADT_A01|MSG002|P|2.5\rEVN||20240101120000\rPID|||123456||Doe^John\r\x1c\x0d' | nc localhost 2576 | tr '\r' '\n'

# Smart handler — response depends on rules.json
printf '\x0bMSH|^~\&|sender|sender|receiver|receiver|20240101120000||ADT^A01^ADT_A01|MSG003|P|2.5\rEVN||20240101120000\rPID|||123456||Doe^John\r\x1c\x0d' | nc localhost 2577 | tr '\r' '\n'
```

> HL7 uses `\r` (carriage return) as the segment separator. Without `| tr '\r' '\n'` the response appears blank in the terminal because each segment overwrites the previous line.

## Configuration

All options can be set via CLI flag or environment variable. CLI flags take precedence over env vars.

| Flag | Env var | Default | Description |
| --- | --- | --- | --- |
| `--host` | `HOST` | `0.0.0.0` | Interface to bind |
| `--ack-port` | `ACK_PORT` | `2575` | Port for the always-ACK handler |
| `--chaos-port` | `CHAOS_PORT` | `2576` | Port for the always-NACK chaos handler |
| `--smart-port` | `SMART_PORT` | `2577` | Port for the rule-based smart handler |
| `--rules-file` | `RULES_FILE` | `rules.json` | Path to the smart handler rules JSON file |
| `--save-path` | `SAVE_PATH` | _(empty)_ | Directory to save every inbound message to. Saving is off when empty |

## Saving Inbound Messages

Point `--save-path` at a directory and MLLPong writes every message it receives — on all three ports — to its own file. The directory is created at startup if it does not exist.

```bash
mllpong --save-path ./files
```

Each file is the raw HL7 payload with the MLLP framing bytes stripped, named so that the files sort chronologically and stay identifiable:

```
files/
├── 20240101-120000.512-000001-ADT-A01-MSG001.hl7
├── 20240101-120001.037-000002-ORU-R01-MSG002.hl7
└── 20240101-120002.884-000003-unknown-unknown.hl7
```

The name is `<timestamp>-<sequence>-<message type>-<control ID>.hl7`. Nothing is ever overwritten: the sequence number keeps concurrent connections apart even when a sender replays the same control ID. Messages with no parsable `MSH` segment are still saved, under `unknown-unknown`.

Under Docker, set `SAVE_PATH` to a path inside the container and mount a volume there so the files land on the host — see the Docker and Docker Compose examples under [Running MLLPong](#running-mllpong).

## Smart Handler

The smart handler (port `2577`) reads a JSON rules file at startup and matches each incoming message against the rules to decide the response. It supports per-message-type configuration, custom acknowledgment codes, artificial latency, and probabilistic chaos.

#### ◉ Rule file format

```json
{
  "rules": [
    {
      "match": "ADT^A01",
      "response": "AA",
      "ack_text": "Patient admitted"
    },
    {
      "match": "ORM",
      "response": "AE",
      "error_code": 207,
      "error_severity": "E",
      "error_msg": "Order processing failed"
    },
    {
      "match": "ADT^A08",
      "response": "AA",
      "nack_rate": 0.2,
      "ack_text": "Patient updated"
    },
    {
      "match": "SIU",
      "response": "AA",
      "delay_ms": 500
    },
    {
      "match": "*",
      "response": "AA",
      "ack_text": "Message accepted"
    }
  ]
}
```

> See [rules.json](https://github.com/novalagung/mllpong/blob/master/rules.json) for complete example.

#### ◉ Rule fields

| Field | Type | Description |
| --- | --- | --- |
| `match` | string | Message type to match. See matching rules below. |
| `response` | string | Acknowledgment code: `AA` (accept), `AE` (error), or `AR` (reject). |
| `ack_text` | string | Free text placed in `MSA.3`. Only meaningful for `AA` responses. |
| `error_code` | int | HL7 error code in the `ERR` segment. Defaults to `207`. |
| `error_severity` | string | Severity in the `ERR` segment: `E` (error), `W` (warning), `F` (fatal). Defaults to `E`. |
| `error_msg` | string | Diagnostic message in the `ERR` segment. |
| `delay_ms` | int | Artificial response latency in milliseconds. |
| `nack_rate` | float | Probability (`0.0`–`1.0`) to override the configured response with `AR`. Useful for simulating intermittent failures. |

#### ◉ Match priority

Rules are evaluated in this order — the most-specific match wins:

1. **Exact** — `"ADT^A01"` matches only ADT messages with trigger event A01
2. **Type** — `"ADT"` matches any ADT message not covered by an exact rule
3. **Wildcard** — `"*"` matches any message not covered by type or exact rules

Match values are case-insensitive. If no rule matches at all, the server defaults to `AA`.

#### ◉ Included `rules.json`

The repository ships with a `rules.json` that covers the most common HL7 v2 message types out of the box:

- **ADT** — A01 through A51 (admit, transfer, discharge, update, merge, link, cancel variants)
- **ORM / ORU / ORL** — order and observation messages
- **SIU** — scheduling (S12–S26)
- **MDM** — document management (T01, T02, T05, T11)
- **MFN** — master file notifications
- **DFT / BAR** — financial and billing messages
- **VXU / PPR / QRY** — vaccination, problem, and query messages
- **`*`** — wildcard fallback returning `AA` for anything not listed above

You can replace or extend this file without rebuilding the image.

## Running Tests

The test suite is split into two layers:

**Unit + coverage tests** — no running server required, uses in-process TCP listeners:

```bash
go test ./...
go test ./... -cover   # with coverage report
```

**Integration tests** — connects to the real server on the configured ports:

```bash
go test -tags integration ./...
```

Override the default target addresses with environment variables:

| Variable | Default | Description |
| --- | --- | --- |
| `TARGET_HOST` | `localhost` | Host to connect to |
| `ACK_PORT` | `2575` | ACK handler port |
| `CHAOS_PORT` | `2576` | Chaos handler port |
| `SMART_PORT` | `2577` | Smart handler port |

## Local Build

**Build then run the image:**

```bash
docker build -t mllpong:local .
docker run -d \
  -e ACK_PORT=2575 \
  -e CHAOS_PORT=2576 \
  -e SMART_PORT=2577 \
  -e RULES_FILE=/etc/hl7/rules.json \
  -p 2575:2575 \
  -p 2576:2576 \
  -p 2577:2577 \
  -v ./rules.json:/etc/hl7/rules.json:ro \
  mllpong:local
```

**Or use Docker Compose with a local build** (already the default in the included `docker-compose.yml`):

```bash
docker compose up -d --build
```

## License

MIT License

## Maintainer

Noval Agung Prayogo
