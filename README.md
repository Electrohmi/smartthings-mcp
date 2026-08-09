# Lango SmartThings MCP Server

[![smithery badge](https://smithery.ai/badge/@langowarny/smartthings-mcp)](https://smithery.ai/server/@langowarny/smartthings-mcp)

A **Model Context Protocol (MCP)** server that exposes Samsung **SmartThings Public API** as
LLM-friendly tools, resources and real-time events.

![Architecture](docs/assets/architecture.svg)

## Features

* **Lazy Loading**: Tools are discoverable without authentication - only validates API keys when tools are invoked
* Wraps common SmartThings operations as **MCP Tools**
  * **Devices**: `list_devices`, `list_devices_with_status`, `get_device`, `get_device_status`, `list_device_capabilities`, `send_device_command`
  * **Locations & Rooms**: `list_locations`, `list_rooms`, `create_room`, `delete_room`
  * **Scenes & Rules**: `list_scenes`, `execute_scene`, `list_rules`
  * **Hubs**: `list_hubs`, `get_hub_health`
  * **Subscriptions**: `list_subscriptions`, `create_subscription`, `delete_subscription`
  * **Schedules**: `list_schedules`, `create_schedule`, `delete_schedule`
  * **History**: `get_device_history`
  * **Capabilities**: `get_capability`
* Exposes device / status / location data as **MCP Resources** with read-through cache
* Supports all official **MCP-Go transports**
  * **Stdio** (CLI / local), **StreamableHTTP**, **Server-Sent Events (SSE)**
* Periodic poller publishes live device status to SSE clients
* Zero external dependencies apart from `mcp-go` and `zap` logger

## Requirements

* Go ≥ 1.23
* A valid **SmartThings PAT** (Personal Access Token)

### Getting a Personal Access Token (PAT)

1. Go to [SmartThings Personal Access Tokens](https://account.smartthings.com/tokens).
2. Log in with your Samsung Account.
3. Click **Generate new token**.
4. Enter a name for your token and select the authorized scopes (e.g., `devices`, `locations`, `scenes`, `rules`, `schedules`).
5. Click **Generate token**.
6. **Copy and save** the token immediately (it won't be shown again).

## Environment Variables

| Name | Default | Description |
|------|---------|-------------|
| `smartThingsToken`, `SMARTTHINGS_TOKEN` | – | Bearer token for SmartThings API. **Required for SmartThings operations**, but server will start without it for tool discovery |
| `stBaseUrl`, `ST_BASE_URL` | `https://api.smartthings.com` | Override for testing / mock servers |
| `MCP_ACCESS_TOKEN` | – | Shared secret that gates the **SSE**/**StreamableHTTP** endpoints. If set, every request must supply it via the `mcpAccessToken` query parameter or an `Authorization: Bearer` header, or it's rejected with `401`. **Strongly recommended whenever the server is reachable from outside your LAN** — without it, anyone who can reach the URL can control your devices using the token baked into the container. Not used by `stdio`. |
| `SMARTTHINGS_LOCATION_ID` | – | Restricts every tool and resource to a single SmartThings location (find IDs via `list_locations`). If your account has multiple homes, set this so the server can only see/control devices, scenes, and rooms in that one location — `list_locations` itself only returns the configured location, and any device/scene/room ID outside it is rejected. Fixed server-side config only; it cannot be overridden via a request, even on `sse`/`stream`. |
| `MCP_LOG_LEVEL` | `info` | `debug` | `info` | `warn` | `error` |

## Installation

### Installing via Smithery

To install smartthings-mcp for Claude Desktop automatically via [Smithery](https://smithery.ai/server/@langowarny/smartthings-mcp):

```bash
npx -y @smithery/cli install @langowarny/smartthings-mcp --client claude
```

### Manual Installation
```bash
git clone https://github.com/langowarny/smartthings-mcp.git
cd smartthings-mcp
go mod download
```

### Container Image (GHCR)

Every push to `main` (and every `vX.Y.Z` tag) is built by [`.github/workflows/docker-publish.yml`](.github/workflows/docker-publish.yml)
and published to the GitHub Container Registry as a multi-arch (`linux/amd64`, `linux/arm64`) image:

```
ghcr.io/electrohmi/smartthings-mcp:latest
```

Available tags: `latest` (tracks `main`), `vX.Y.Z` / `X.Y` (release tags), and `sha-<short-sha>` (every build).

```bash
docker pull ghcr.io/electrohmi/smartthings-mcp:latest
docker run -d --name smartthings-mcp \
  -p 8081:8081 \
  -e SMARTTHINGS_TOKEN=123ab456-xxx... \
  -e MCP_ACCESS_TOKEN=$(openssl rand -hex 32) \
  --restart unless-stopped \
  ghcr.io/electrohmi/smartthings-mcp:latest
```

> **If this port/domain is reachable from outside your LAN, always set `MCP_ACCESS_TOKEN`.**
> Without it, the SSE/StreamableHTTP endpoints accept unauthenticated requests and execute
> them using the `SMARTTHINGS_TOKEN` baked into the container — anyone with the URL could
> control your devices. See [Environment Variables](#environment-variables) and the Synology
> section below for how to wire the token through to Claude.

### Synology NAS (Container Manager)

To run the server on a Synology NAS via **Container Manager** (DSM 7.2+), pulling the image built above:

1. **Container Manager → Registry**: search `ghcr.io/electrohmi/smartthings-mcp` and download the `latest` tag
   (or add `ghcr.io` as a custom registry under *Registry → Settings* if it isn't listed by default).
   If the GHCR package is private, log in first: *Registry → Settings → Add* with your GitHub username and a
   [PAT with `read:packages` scope](https://docs.github.com/en/packages/working-with-a-github-packages-registry/working-with-the-container-registry#authenticating-to-the-container-registry) as the password.
2. **Container Manager → Project**: create a new project, choose *Upload docker-compose.yml*, and upload the
   [`docker-compose.yml`](docker-compose.yml) from this repo (or paste its contents).
3. Set `SMARTTHINGS_TOKEN` and `MCP_ACCESS_TOKEN` (Container Manager project *Environment* tab, or a `.env`
   file next to `docker-compose.yml`):
   ```
   SMARTTHINGS_TOKEN=123ab456-xxx...
   MCP_ACCESS_TOKEN=<a long random secret, e.g. output of `openssl rand -hex 32`>
   ```
   then build/start the project.
4. The server listens on port `8081` (StreamableHTTP transport). Behind a reverse proxy
   (e.g. `https://electrohmi.synology.me/smartthings/`), your MCP client must hit the exact
   proxied URL with `MCP_ACCESS_TOKEN` appended as a query parameter:
   ```
   https://electrohmi.synology.me/smartthings/?mcpAccessToken=<your MCP_ACCESS_TOKEN>
   ```
   This is required because Claude's custom connector UI doesn't support attaching a static
   header/API key — the token has to travel in the URL itself. Requests missing or with an
   incorrect token get `401 Unauthorized`; without `MCP_ACCESS_TOKEN` configured at all, the
   endpoint stays open to anyone who can reach it.

Alternatively, from an SSH session on the NAS you can run the same `docker pull` / `docker run` commands shown
above, or `docker compose up -d` using the provided `docker-compose.yml`.

#### Troubleshooting: tools fail with a DNS/lookup or connection-timeout error

If tool calls fail with something like
`dial tcp: lookup api.smartthings.com on 127.0.0.11:53: ... i/o timeout` (or the container has no
`eth0` at all in `docker exec smartthings-mcp ip addr show`), the actual cause on Synology is
usually **DSM's built-in firewall (Control Panel → Security → Firewall) not allowing the Docker
project's private subnet**, not a block on the SmartThings domain itself.

Each `docker compose` project gets its own auto-generated bridge network (e.g.
`smartthings-mcp_default`) with a subnet Docker picks from its `192.168.0.0/16` pool — and that
subnet can change every time the network is recreated (container/NAS restart, `docker compose
down && up`, etc.). If DSM's firewall profile only allows specific subnets (commonly added ad hoc
per Docker project over time), a newly-allocated subnet that isn't on the list gets silently
dropped before it ever reaches the internet.

**Fix**: in DSM, Control Panel → Security → Firewall → edit your profile → **Create** a rule
allowing the whole Docker private range instead of one-off subnets:
- Port: All
- Protocol: All
- Source IP: `192.168.0.0`, subnet mask `255.255.0.0`
- Action: Allow

Move it above any catch-all deny rule and apply. This covers every subnet Docker could ever
allocate in that pool, so it won't need to be re-added when a project's network gets recreated.

To confirm the container's network endpoint itself is healthy (separate from the firewall issue),
fully recreate it rather than just restarting the container:
```bash
sudo docker compose down && sudo docker compose up -d
sudo docker exec smartthings-mcp ip addr show          # should show eth0 with a 192.168.x.x address
sudo docker exec smartthings-mcp nslookup api.smartthings.com
```

## Running

### Stdio

```bash
SMARTTHINGS_TOKEN=123ab456-xxx... go run ./cmd/server -transport stdio
```

### StreamableHTTP

```bash
SMARTTHINGS_TOKEN=123ab456-xxx... \
  go run ./cmd/server -transport stream -host 0.0.0.0 -port 8081
```

Test request:

```bash
curl -X POST http://localhost:8081/mcp/tools/call \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_devices"}}'
```

### SSE

```bash
SMARTTHINGS_TOKEN=123ab456-xxx... \
  go run ./cmd/server -transport sse -host 0.0.0.0 -port 8081

# Open event stream
echo -e 'GET /mcp/sse HTTP/1.1\nHost: localhost:8081\n\n' | nc localhost 8081
```

The server emits `smartthings/device_status` notifications every 30 seconds.

## Tool Catalogue

| Tool | Params | Description |
|------|--------|-------------|
| `list_devices` | `location_id?` | List user devices |
| `list_devices_with_status` | `location_id?`, `category?`, `capability?` | List devices with live status, fetched in parallel — use this instead of `list_devices` + `get_device_status` per device when asking about many devices at once (e.g. "what lights are on"); one round trip instead of one per device. Filter with `category` (e.g. `Light`) whenever you only care about one kind of device — on accounts with many devices/complex appliances, an unfiltered call can return a very large response |
| `get_device` | `device_id` | Device metadata |
| `get_device_status` | `device_id` | Live status |
| `list_device_capabilities` | `device_id` | Supported capabilities |
| `send_device_command` | `device_id`, `component`, `capability`, `command`, `arguments?[]` | Issue command |
| `list_locations` | – | List locations |
| `list_rooms` | `location_id` | List rooms in a location |
| `create_room` | `location_id`, `name` | Create a new room |
| `delete_room` | `location_id`, `room_id` | Delete a room |
| `list_scenes` | – | List all scenes |
| `execute_scene` | `scene_id` | Trigger scene |
| `list_rules` | – | List automation rules |
| `list_hubs` | – | List hubs |
| `get_hub_health` | `hub_id` | Get hub health status |
| `list_subscriptions` | `installed_app_id` | List subscriptions |
| `create_subscription` | `installed_app_id`, `device_id`, ... | Subscribe to device events |
| `delete_subscription` | `installed_app_id`, `subscription_id` | Delete subscription |
| `list_schedules` | `installed_app_id` | List schedules |
| `create_schedule` | `installed_app_id`, `name`, `cron` | Create cron schedule |
| `delete_schedule` | `installed_app_id`, `schedule_id` | Delete schedule |
| `get_device_history` | `device_id` | Get recent device events |
| `get_capability` | `capability_id`, `version` | Get capability definition |

## Resource Patterns

| URI Template | Description | MIME |
|--------------|-------------|------|
| `st://devices/{device_id}` | Device metadata | `application/json` |
| `st://devices/{device_id}/status` | Live status | `application/json` |
| `st://locations/{location_id}` | Location metadata | `application/json` |

## Development

```bash
go vet ./...
go test ./...
go run ./cmd/server -transport stream
```

Logs are emitted via **Uber Zap**; adjust `MCP_LOG_LEVEL` for verbosity.

## License

MIT © 2025 Lango Warny
