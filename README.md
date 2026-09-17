# PhonArch Conference

PhonArch Conference is a multi-tenant, moderated telephone conference platform for large one-to-many voice rooms. A host speaks to a large population of mobile callers. Callers are muted by default, can press `0` to request the floor, and can press `#` to withdraw the request. An operator approves one or more callers from the room control panel. Approved callers become speakers; everybody else continues to receive the broadcast and remains muted.

The product is deliberately phone-first. It uses SIP signaling and RTP media. The browser is an HTTP administration console; it is not a media endpoint.

## Repository topology

The repository follows the same component-oriented layout as the Phonarch voice
platform:

```text
components/
  phonarch-platform-api/       Go control API and PostgreSQL migrations
  phonarch-platform-web/       Next.js operator console (`src/app`)
  phonarch-sip-gateway/        Go/sipgo public SIP edge
  phonarch-pbx-sidecar/        Go per-core command and heartbeat service
  phonarch-rustpbx/            RustPBX adapter contract and heartbeat utility
  phonarch-rustpbx-lab/        x86_64/ARM64 mock core for local validation
  phonarch-sip-probe/          SIP/RTP lab probe
deploy/                        native build/start/stop/status scripts and env template
docs/                          architecture and adapter documentation
state/config/                  versioned host topology/configuration
state/secrets/                ignored deployment credentials
state/runtime/                ignored logs, PIDs, Redis, and local runtime data
```

Every component owns its source, module/package manifest, tests, and generated
`bin/` output. The deploy scripts orchestrate components but do not contain
service implementation code.

## Current delivery status

This repository contains a native lab-ready control plane, UI, SIP edge, sidecar contract, PostgreSQL tenancy model, Redis leases, and RustPBX adapter configuration. It also contains a mock RustPBX so the UI and call state workflow can be exercised on x86_64.

The real RustPBX media engine remains an external executable. The files under `/data/components/phonarch-rustpbx` describe the production configuration and the media-control contract; they do not pretend to be a new mixer implementation. A production rollout must connect the sidecar contract to the RustPBX build that owns SIP/RTP, conference mixing, RTP fan-out, DTMF extraction, recording policy, and media failover.

The HA model below is the target production design. The code already provides the state, routing, lease, fencing, and control boundaries required to implement it, but a lab with the mock engine is not proof of 10,000 concurrent calls or sub-second audio failover.

## Product model

### Workspace isolation

A workspace is the SaaS tenant boundary. Every customer-facing object is scoped to one workspace:

- conference rooms (`bridges`);
- participants and their call legs;
- bulk dial batches and batch items;
- sidecar commands;
- room media sessions and leaf assignments;
- speaker requests and room events.

The authenticated API session carries the workspace ID. The API does not trust an arbitrary `X-Workspace-ID` header for customer authorization. Every read, write, action, and room command adds the session workspace predicate. A future workspace switch must re-issue a session only after checking `workspace_members`.

The seeded local workspace is `Operations`. It exists so the current admin can log in immediately. The UI currently presents that default workspace, while the API and database already enforce the tenant boundary needed for a workspace switcher and RBAC expansion.

### Room isolation

A room is one logical conference bridge inside one workspace. A participant is assigned to a room before an outbound call is dispatched. A room's live participant list is built only from that room's call legs; an idle room does not show infrastructure heartbeats or connected callers from another room.

The room is also the media-control and failure-fencing boundary. Media commands carry `workspace_id`, `room_id`, `room_epoch`, `node_epoch`, and an idempotent `command_id`. A stale command from an old root or leaf assignment must be rejected by the media engine rather than changing a newly assigned room.

### Customer workflow

1. An operator logs in to a workspace.
2. The operator creates a conference room.
3. A direct dial or an XLSX/CSV batch attaches contacts to that room.
4. The control API selects a healthy PBX node and creates a participant and call leg before dispatch.
5. SipGo routes the SIP dialog to the selected PBX and preserves affinity for every in-dialog request.
6. The participant is muted by default after answer.
7. The host broadcast is delivered to listeners through the room's media tree.
8. A caller presses `0`; the DTMF event enters the room's speaker queue.
9. The operator grants or dismisses the request. Granting sends an idempotent unmute/media command to the call's owning PBX.
10. The operator can mute, inspect volume, or drop a participant. Dropped participants remain in room history and can be called again.

## Protocol and deployment rules

- SIP over UDP and TCP is supported at the edge.
- RTP/RTCP is the media transport between phones, leaves, and roots.
- SIP INFO DTMF is handled at the edge; production must also support RFC 4733 telephone-event because carriers and mobile gateways vary.
- Browser traffic is ordinary HTTP. The UI does not use WebRTC, WSS, browser RTP, or browser microphone access.
- Redis, PostgreSQL, Go services, RustPBX, and Next.js run as native host processes.
- Docker, Kubernetes, and container-specific networking are intentionally not part of this platform.
- Source, configuration, build output, runtime data, and runner scripts are under `/data`.

## Target production topology

The system is divided into four planes.

```text
                    SIP UDP/TCP
Provider SBC / PSTN --------------------+
                                         v
                           SIP-aware edge tier
                    +-----------------------------+
                    | SipGo edge pool (N >= 2)    |
                    | Redis leases + DTMF         |
                    +---------------+-------------+
                                    |
                    Redis HA: leases, affinity, room state
                                    |
                         Control API / command workers
                                    |
                 +------------------+------------------+
                 |                                     |
          Root media pair                         Leaf media pool
          Root A + Root B                         Leaf 01 ... Leaf N
                 |                                     |
                 +----------- RTP room tree -----------+
                                    |
                         phones / mobile gateways

       PostgreSQL: durable tenant, room, call, command and event state
       Next.js UI: HTTP operator console only
```

### Control plane

The control plane owns identity, workspace authorization, room configuration, participant lifecycle, node selection, command idempotency, room epochs, speaker queue state, audit events, and reconciliation. It does not mix RTP.

### SIP edge plane

SipGo is a SIP edge proxy and load balancer. Any number of identical SipGo edge processes can run on separate SIP interfaces/hosts. Each process advertises itself in Redis as `active-sipgo:<derived-or-configured-id>` with a short TTL. Core sidecars discover those edge advertisements and send their OPTIONS heartbeat to every live edge. The edges share Redis PBX leases and dialog affinity, so a provider can send initial dialogs to either edge and later in-dialog requests can be recovered by the other edge.

The two-NIC lab uses private binds `172.31.10.39:5060` and `172.31.10.5:5060`. Their cloud public mappings are `100.24.65.234` and `54.84.128.150`. SipGo binds the private interface address because cloud public addresses are normally NAT mappings; the provider still reaches the corresponding public SIP address. In a multi-host deployment, each edge binds its own host-private address on UDP/TCP 5060.

New calls are selected using capacity and reported load score. The edge excludes nodes with `DRAINING` or `DRAINED` state and excludes nodes at their hard call capacity. This is weighted least-load with round-robin tie breaking, not blind round-robin.

### Media plane

The media plane is a cascade, not a 10,000-way full mixer:

- a root mixes the host and currently approved speakers;
- roots publish a stable broadcast RTP stream for a room;
- leaves duplicate the broadcast packets to their local listeners;
- leaves drop muted callers' inbound RTP before it consumes root bandwidth;
- an approved speaker's RTP is forwarded upstream to the root;
- the root creates the normal broadcast stream and a mix-minus stream for that speaker;
- the speaker receives the mix-minus stream while everybody else receives the broadcast.

The media engine must preserve downstream SSRC, sequence continuity, timestamps, and codec/ptime expectations across a root switch. A receiving phone should see a short packet gap, not a new call.

### Data plane

PostgreSQL is the source of truth for customer-visible and recovery-critical state. Redis is the low-latency lease, affinity, and live coordination layer. Redis keys have TTLs and must never be the only durable record of a call or room.

## Root A/B failover

Each active room is assigned a root pair and a monotonically increasing `room_epoch`.

1. A speaker's ingress is duplicated to Root A and Root B while both are healthy.
2. Both roots mix the same room inputs and publish the room's broadcast stream.
3. Leaves listen to the active root. The RTP selector owns the downstream socket and emits one stable SSRC/sequence space.
4. The selector switches from Root A to Root B after a bounded packet timeout, for example 300 ms, only after validating the new stream's room epoch and source identity.
5. A root that comes back must not become active until it has caught up and has been explicitly promoted.
6. If both roots disagree about the epoch, the selector fails closed for that room and the control plane raises an alert rather than broadcasting two conflicting rooms.

The selector is important. Simply sending Root A and Root B packets to leaves and hoping each phone handles duplicate SSRCs is not production-safe. The selector must deduplicate, fence old epochs, and preserve downstream RTP continuity.

## Leaf failover

The original proposal to issue a re-INVITE to every phone when a leaf dies is not a safe primary mechanism. SipGo cannot impersonate a dead PBX's dialog or guarantee that thousands of carrier-held dialogs accept a mass re-INVITE. Re-INVITE is a recovery fallback, not the normal failure path.

The production preference is:

1. expose a stable media VIP/anycast or a stateful media relay identity for a leaf pool;
2. keep the phone's negotiated RTP endpoint stable while a healthy relay takes ownership;
3. replicate leaf assignment and media session state before failure;
4. use a targeted SIP re-INVITE only when the phone's SDP endpoint must change;
5. rate-limit recovery re-INVITEs and retry by dialog, not as one unbounded broadcast storm.

If the network design cannot provide a stable media identity, the platform must accept a short silence window and a non-zero subset of carrier endpoints that drop the call. That behavior must be measured with the actual carriers before promising zero dropped calls.

## SipGo HA

SipGo is stateful at the SIP-dialog layer. Two instances behind a generic UDP load balancer are not enough.

Production options, in preference order:

- a SIP-aware Kamailio/OpenSIPS front tier with a floating VIP, dialog-aware routing, and SipGo workers behind it;
- active/standby SipGo with a VIP and Redis-backed affinity;
- an equivalent SIP proxy that preserves source/transaction/dialog consistency for UDP and TCP.

An HTTP load balancer or a random UDP load balancer can split retransmissions and in-dialog requests and must not be used as the only SIP failover strategy.

The current SipGo implementation adds Redis dialog records under `sip-dialog:<call-id>` and tag-qualified keys. This allows a second process to recover the destination for later dialog traffic. A full HA rollout still needs transaction replication, SIP proxy/VIP behavior, duplicate suppression, and a controlled ownership model for the initial INVITE.

## Heartbeat and node selection

Each SipGo edge registers itself in Redis with a seven-second lease by default. Each core sidecar discovers `active-sipgo:*` from Redis and sends a UDP SIP OPTIONS packet to every discovered edge every two seconds. Static heartbeat targets remain only as an optional bootstrap fallback. The packet contains:

- node ID, private IP, SIP UDP/TCP ports, and sidecar control URL;
- node role (`root` or `listener`) and drain state;
- hard capacity and active calls;
- active RTP sessions and RTP packets per second;
- active speakers and active bridges;
- load score and node epoch.

Every edge stores each complete core report at `active-pbx:<node_id>` with a seven-second TTL by default. If a sidecar, core host, or one edge fails, the other edge can still receive the same report. Redis expires a core key without requiring cleanup. A graceful sidecar shutdown removes its key, but the TTL is the safety net. The edge advertisement contains the private SIP address used by sidecars, the public SIP address used by the provider, the HTTP control URL, and the edge epoch.

The heartbeat interval must be materially smaller than the lease. The production rule is at least three missed heartbeats before declaring a node unavailable. The edge must not route new calls to an expired or draining node. Existing dialogs require media/control reconciliation and are not magically repaired by the lease alone.

Load distribution should use the following hierarchy:

1. discard expired, malformed, draining, or capacity-exhausted nodes;
2. constrain by role and room assignment;
3. score calls, RTP sessions, packets per second, active speakers, CPU, memory, and network headroom;
4. select the lowest score with deterministic tie breaking;
5. increment an admission reservation in Redis or PostgreSQL before dispatch so concurrent edge workers cannot all select the last slot.

The current scaffold performs steps 1, 3, and 4 using heartbeat-reported capacity/load. An atomic admission reservation is a production follow-up before high-rate multi-edge origination.

## DTMF and moderated speaking

### Listener request

The preferred signaling path is an in-dialog SIP INFO handled by SipGo. SipGo returns a fast 200 response to the phone/carrier, then posts an internal event to the control API. The event includes Call-ID, From/To tags, digit, workspace, room, and participant hints. The API resolves the call leg from its durable database and does not trust the hints to cross a tenant boundary.

The accepted digits are:

- `0`: create one open `speaker_requests` row, mark the participant `HAND_RAISED`, and append a `SPEAKER_REQUESTED` room event;
- `#`: withdraw an open request, set the participant back to `MUTED`, and append a withdrawal event.

The production RustPBX/SIP path must additionally normalize RFC 4733 telephone-event DTMF. In-band audio tone recognition should not be the authoritative control path for a large room.

### Operator grant

The UI calls the authenticated room endpoint. The API:

1. checks workspace and room ownership;
2. enforces `ROOM_MAX_ACTIVE_SPEAKERS` (default 16);
3. reads the participant's durable call-leg owner and node epoch;
4. verifies that the owning node lease is live;
5. sends an idempotent `unmute` command to that sidecar;
6. updates the request and participant state only after command acceptance;
7. records `SPEAKER_GRANTED` with the command ID.

If the PBX owner is gone, the grant returns a conflict instead of claiming success. A reconciler should later reassign or redial the participant according to product policy.

### Mix-minus behavior

For an approved speaker, the root publishes:

- broadcast: host plus all currently approved speakers;
- mix-minus for speaker X: broadcast minus X's own RTP.

When X is muted, the leaf cuts X's upstream RTP and the root removes X's mix-minus stream. The downstream stream remains continuous and the caller returns to listener mode.

## Persistence model

The migrations are applied by `/data/deploy/scripts/init-db.sh`:

- `001_initial.sql`: operators, bridges, participants, call legs, dial batches, and node commands;
- `002_room_delete_constraints.sql`: safe room deletion behavior, including the participant/batch-item foreign-key issue;
- `003_multitenant_media_ha.sql`: workspaces, workspace membership, workspace foreign keys, media sessions, leaf assignments, speaker requests, room events, epochs, and command metadata.

Important relationships:

```text
workspace
  └── bridge / conference room
        ├── participants
        │     └── call_legs -> node_id + node_epoch
        ├── dial_batches -> dial_batch_items
        ├── room_media_sessions -> active/standby roots + room_epoch
        ├── room_leaf_assignments -> leaf node/stream ownership
        ├── speaker_requests
        └── room_events
```

Do not hard-delete a participant that is still referenced by `dial_batch_items`. The room-delete migration uses the correct relationship behavior; deletion must still be performed through the API so active calls are dropped and audit history is preserved.

The current sidecar keeps short-lived call and command caches in memory for the local lab. Production needs a durable command outbox and reconciliation worker:

1. insert an intent and command ID in PostgreSQL;
2. deliver it to the sidecar;
3. retry safely using the same ID;
4. record accepted/applied/failed status;
5. reconcile DB state with PBX state after a sidecar or API restart.

Without this outbox, a sidecar restart can lose its in-memory `PBXMemberID` lookup even though the database still knows the call leg exists.

## Service contracts in this tree

### SipGo edge: `/data/components/phonarch-sip-gateway`

- SIP UDP/TCP listener: `SIPGO_LISTEN` and `SIPGO_TCP_LISTEN`, normally private interface `:5060` mapped to a public SIP address.
- Dynamic edge advertisement: `active-sipgo:<edge_id>` in Redis; `SIPGO_NODE_ID` is optional and otherwise derived from `SIPGO_ADVERTISE_IP`.
- Health: `GET /healthz`.
- Readiness: `GET /readyz`.
- Node inventory: `GET /internal/v1/nodes`.
- Graceful deregistration: `POST /internal/v1/nodes/deregister`.
- Go dependency: `github.com/emiago/sipgo`.

Supported edge methods are OPTIONS, INVITE, ACK, BYE, CANCEL, UPDATE, and INFO. INVITEs with a To-tag are treated as in-dialog re-INVITEs and are never selected as a new call.

### PBX sidecar: `/data/components/phonarch-pbx-sidecar`

The sidecar runs beside one RustPBX instance and is the only control boundary the API needs to know. It sends SIP OPTIONS and forwards commands to the local RustPBX HTTP control endpoint.

- `GET /healthz`, `GET /readyz`;
- `POST /v1/calls/originate`;
- `GET /v1/calls/{call_id}`;
- `POST /v1/participants/{id}/mute`;
- `POST /v1/participants/{id}/unmute`;
- `POST /v1/participants/{id}/drop`;
- `POST /v1/rooms/{room_id}/media/{attach|publish|unpublish|subscribe|unsubscribe|failover}`.

Every command should include an idempotency key, workspace ID, room ID, room epoch, and node epoch. The adapter contract is documented in [`docs/RUSTPBX-ADAPTER-CONTRACT.md`](/data/docs/RUSTPBX-ADAPTER-CONTRACT.md).

### Control API: `/data/components/phonarch-platform-api`

The API owns authenticated customer operations:

- login/logout and current workspace;
- room CRUD;
- direct dial and bulk batch dispatch;
- room participant state and actions;
- volume reads;
- speaker request queue, grant, and withdraw;
- internal DTMF and sidecar event ingestion.

The API binds to localhost by default. If it is exposed beyond the host, configure a real TLS/reverse-proxy boundary, a non-empty `PHONARCH_INTERNAL_TOKEN`, an explicit `UI_ORIGIN`, rate limits, and production identity/RBAC. The scaffold's admin username/password are lab credentials only.

### UI: `/data/components/phonarch-platform-web`

The UI is Next.js App Router + TypeScript + Tailwind configuration + Lucide icons. It contains:

- workspace landing page with room cards and room deletion confirmation;
- direct SIP dialer;
- local XLSX/CSV preview and controlled batch submission;
- full-height room sidebar;
- live participant controls for mute, unmute, volume, and drop;
- moderated speaker-request queue with Allow and Dismiss actions;
- activity and room settings surfaces.

The CSS variables and material rules are in `/data/components/phonarch-platform-web/src/app/globals.css`: cool off-white background, frosted glass panels, mint gradients, green brand buttons, and the requested text/border/status tokens.

The canonical visual tokens are `--background: #f5f7f8`, `--surface: rgba(255,255,255,.86)`, `--surface-strong: rgba(255,255,255,.95)`, `--text-primary: #182732`, `--text-secondary: #43535e`, `--text-muted: #71808b`, `--border: rgba(38,56,68,.14)`, `--accent: #147b63`, `--accent-deep: #0d5c4b`, `--accent-soft: #e3f3ed`, `--accent-wash: #f0f8f5`, `--success: #347d59`, `--warning: #a76c32`, `--error: #a14d4d`, and `--info: #527daf`. Glass panels use an 18px backdrop blur, a 12px/34px cool shadow, and the specified translucent border. Brand actions use the mint-to-deep-green diagonal gradient. These are customer-operator surfaces, not media surfaces.

## RustPBX configuration

`/data/components/phonarch-rustpbx/config.toml` defines the production intent:

- listener/root role;
- soft capacity 350 and hard capacity 500 for an initial four-CPU listener profile;
- drain state;
- 16 active speakers per room by default;
- root failover timeout and stable downstream SSRC requirement;
- room epoch fencing.

The values are starting admission limits, not a vendor guarantee. Capacity depends on codec, ptime, transcoding, TLS/SRTP choices, recording, packet rate, NIC, kernel tuning, and whether the node is a leaf, root, or mixed role. Benchmark the exact RustPBX build with the target carrier codecs before changing the limit.

Recommended initial roles:

- listener leaf: many calls, no full-room mix, UDP fan-out only;
- root mixer: fewer sessions, high packet and CPU budget, paired active/standby;
- control/edge: SIP transactions and commands, not RTP mixing.

Do not put roots and high-density leaves on the same four-CPU host until a measured benchmark shows sufficient headroom.

## Failure matrix

| Failure | Expected behavior | Required production component |
| --- | --- | --- |
| Sidecar process dies | Redis lease expires; no new calls route there | 2–3 heartbeat misses, node drain, reconciliation |
| Graceful PBX drain | heartbeat says `DRAINING`; new calls avoid node | operator/auto-drain and admission fencing |
| SipGo process dies | SIP-aware front tier sends new traffic to peer; Redis dialog affinity survives | transaction/dialog ownership and duplicate handling |
| Redis unavailable | edge cannot safely select or renew nodes | Redis HA/sentinel/cluster and fail-closed admission |
| PostgreSQL unavailable | API rejects writes; existing media should continue | DB HA, pool timeouts, outbox retry |
| Root A dies | RTP selector changes to Root B after validated timeout | duplicated ingress, root selector, stable SSRC/epoch |
| Root pair split-brain | room is fenced and alerted | epoch/lease ownership, never dual-active roots |
| Leaf dies | stable relay/VIP takes over, or targeted re-INVITE recovery | stateful media relay and carrier-tested re-INVITE fallback |
| Carrier rejects re-INVITE | call may remain on old media or drop | stable media identity, bounded retries, alert |
| Operator double-clicks grant | one command is applied | idempotency key and DB state transition |
| Old room command arrives | command is rejected | workspace + room epoch + node epoch fencing |
| DTMF is lost | participant can retry; request remains auditable | SIP INFO + RFC 4733 normalization and dedupe |
| UI disconnects | calls continue; operator reconnects to durable state | HTTP polling/reconnect and DB event history |

## Scaling and admission plan

Horizontal scale is per media role, not just per process count.

1. Measure one listener node with the exact codec and RTP packet rate.
2. Set a soft admission threshold below CPU, packet, socket, and NIC saturation.
3. Set a hard threshold that SipGo will never exceed.
4. Keep N+1 leaf capacity for a planned failure.
5. Keep two roots per active room or shard, plus spare root capacity.
6. Start a new leaf when the fleet reaches the soft threshold or when forecast capacity falls below the N+1 target.
7. Mark a draining node `DRAINING`, stop new admissions, migrate/recover media ownership, then mark `DRAINED`.
8. Scale down only after active RTP sessions, speakers, room assignments, and command outbox work are zero.

Metrics required before automatic scaling:

- SIP INVITE/transaction latency, retransmits, 4xx/5xx, and dialog count;
- active calls, answered calls, failed calls, calls per node;
- RTP packets per second, loss, jitter, late packets, socket drops, and bandwidth;
- root mix CPU, leaf fan-out CPU, queue depth, and active speakers;
- room epoch changes and root/leaf failovers;
- Redis lease age and PostgreSQL command-outbox lag;
- DTMF requests, grant latency, and stale/duplicate commands.

The `X-PhonArch-*` OPTIONS headers already carry the node-level fields needed by SipGo. They must be populated from real RustPBX and kernel counters before automated admission is trusted; the local scaffold uses placeholders for packet/speaker counters.

## Build and run

### Native prerequisites

For a normal host install, use native Go, Node.js/npm, Redis, PostgreSQL/psql, and the RustPBX executable. No container runtime is needed.

For the current x86_64 lab, native Go and Node toolchains are staged under `/data/toolchain`, and Redis is available under `/data/toolchains/redis/usr/bin`. The scripts automatically prepend these directories if they exist. The same layout can be replaced by ARM64 binaries on an ARM64 host.

### Database

```bash
cp /data/deploy/phonarch-conference.env.example /data/state/secrets/phonarch-conference.env
# edit DATABASE_URL, credentials, SIP addresses, and UI origin
/data/deploy/scripts/init-db.sh
```

The migration is additive and safe to rerun with the included `IF NOT EXISTS`/conflict guards. Back up PostgreSQL before applying it to a real environment.

### Build

```bash
/data/deploy/scripts/build.sh
```

The build detects the host architecture and emits each binary in its owning
component's `/data/components/<component>/bin` directory:

- `sipgo-lb-linux-amd64` or `sipgo-lb-linux-arm64`;
- `pbx-sidecar-linux-amd64` or `pbx-sidecar-linux-arm64`;
- `control-api-linux-amd64`;
- `rustpbx-heartbeat-*` and `mock-rustpbx-*`;
- a Next.js standalone production bundle under `/data/components/phonarch-platform-web/.next/standalone`.

The Go module tests and `npm run build` are the minimum validation gate.

### Start and stop

```bash
/data/deploy/scripts/start.sh
/data/deploy/scripts/status.sh
/data/deploy/scripts/stop.sh
```

The local profile starts Redis, SipGo, two mock RustPBX nodes, two sidecars, the control API, and the UI. It does not start an external RustPBX unless `RUSTPBX_BIN` points to an executable. Runtime logs, PIDs, Redis data, and audit files live under `/data/state/runtime`.

The launcher is list-driven. For example, the current two-NIC lab uses:

```bash
SIPGO_BIND_ADDRESSES=172.31.10.39:5060,172.31.10.5:5060
SIPGO_TCP_BIND_ADDRESSES=172.31.10.39:5060,172.31.10.5:5060
SIPGO_HTTP_ADDRESSES=127.0.0.1:8080,127.0.0.1:8082
SIPGO_ADVERTISE_IPS=172.31.10.39,172.31.10.5
SIPGO_PUBLIC_IPS=100.24.65.234,54.84.128.150
CORE_PRIVATE_IPS=172.31.10.39,172.31.10.5
CORE_MEDIA_PUBLIC_IPS=100.24.65.234,54.84.128.150
CORE_SIP_PORTS=5071,5071
CORE_RUSTPBX_CONTROL_ADDRESSES=172.31.10.39:9090,172.31.10.5:9090
CORE_SIDECAR_ADDRESSES=172.31.10.39:9443,172.31.10.5:9443
```

Adding a third edge or core means adding one entry to the corresponding lists; no `server-01`/`server-02` identity is compiled into the services. Runtime IDs are derived from private addresses and the current advertisement is visible with Redis:

The two core nodes intentionally use the same SIP port (`5071`) and the same
sidecar/control ports (`9090`/`9443`) because each binds a different private
interface address. A port only needs to change when two processes share the
same local IP address, as in a loopback-only mock setup.

```bash
/data/toolchains/redis/usr/bin/redis-cli --scan --pattern 'active-sipgo:*'
/data/toolchains/redis/usr/bin/redis-cli --scan --pattern 'active-pbx:*'
```

For a real RustPBX binary, set `RUSTPBX_CONFIG_PATHS` to one engine configuration path per `CORE_PRIVATE_IPS` entry. The configuration must bind SIP to the private core address and advertise `CORE_MEDIA_PUBLIC_IPS` in SDP/RTP relay configuration. The runner refuses a real engine when the config list length does not match the core list.

The UI listens on `0.0.0.0:${UI_PORT:-3000}` for the lab. Do not expose the control API, sidecars, Redis, PostgreSQL, or RTP administration endpoints directly to the public internet. Put the UI behind an authenticated HTTPS reverse proxy in production; the SIP edge ports are the only public signaling endpoints, while RustPBX SIP/control stays private and RustPBX RTP advertises the configured public media address.

### Lab checks

```bash
curl http://127.0.0.1:8080/healthz
curl http://127.0.0.1:8081/healthz
curl http://127.0.0.1:3000/login
/data/toolchains/redis/usr/bin/redis-cli --scan --pattern 'active-pbx:*'
```

The lab admin credentials come from `/data/state/secrets/phonarch-conference.env`, not from the source code. Change them before any shared deployment.

## Production work remaining

The following items are intentionally called out instead of being hidden behind a green health check:

1. Build or install the real RustPBX engine and implement the room media endpoints in its sidecar adapter.
2. Implement the RTP root selector with stable downstream SSRC/sequence handling and room-epoch fencing.
3. Add a stateful leaf relay/VIP strategy and carrier-tested re-INVITE fallback.
4. Decide whether the carrier will fail over between the advertised public SipGo addresses directly, or deploy a SIP-aware HA front tier/VIP. Two SipGo instances sharing Redis are supported, but Redis does not create a public VIP or repair a provider that only knows one destination.
5. Move sidecar call/command state to a durable command outbox and add a DB-to-PBX reconciler.
6. Populate heartbeat telemetry from real RTP, CPU, memory, NIC, and RustPBX counters.
7. Add RFC 4733 DTMF normalization and test INFO/telephone-event behavior against every carrier.
8. Add RBAC, workspace creation/switching, secret rotation, audit retention, and rate limits.
9. Add Redis HA and PostgreSQL HA/backups/PITR.
10. Run SIP/RTP load, failure, NAT, packet-loss, root-split-brain, carrier-reINVITE, and long-duration tests before a 10k capacity claim.

The most important correctness rule is that workspace ID, room ID, room epoch, node epoch, and command ID travel together through every control operation. The most important media rule is that failover must preserve the phone's RTP identity or explicitly accept the carrier behavior observed in testing. A Redis TTL solves stale node discovery; it does not by itself repair an already-established SIP dialog or reconstruct a lost RTP mixer.

## Source map

- [`phonarch-sip-gateway`](/data/components/phonarch-sip-gateway): SIP edge and Redis-backed routing.
- [`phonarch-platform-api`](/data/components/phonarch-platform-api): authenticated room/call API, coordinator, and PostgreSQL migrations.
- [`phonarch-pbx-sidecar`](/data/components/phonarch-pbx-sidecar): per-PBX control/heartbeat service.
- [`phonarch-rustpbx`](/data/components/phonarch-rustpbx): node configs and adapter contract.
- [`phonarch-platform-web`](/data/components/phonarch-platform-web): Next.js App Router conference console.
- [`deploy`](/data/deploy): native build, database, start, stop, and status runners.
- [`docs`](/data/docs): architecture and integration contracts.
- [`deploy/phonarch-conference.env.example`](/data/deploy/phonarch-conference.env.example): environment contract.
