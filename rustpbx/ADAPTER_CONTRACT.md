# RustPBX adapter contract

The Go sidecar is the stable control boundary around the supplied RustPBX engine. RustPBX must expose a private HTTP JSON interface or an equivalent local adapter.

Core nodes are discovered dynamically through Redis. The node identity is derived
from the configured private address unless an orchestrator supplies an explicit
ID. SIP/control listeners stay on the core's private address. RTP SDP/relay
advertising uses the node's separately configured public media address; these
addresses must not be conflated with the private SIP address.

## Required engine operations

```text
POST /v1/calls/originate
POST /v1/bridges
POST /v1/bridges/{bridge_id}/participants
POST /v1/participants/{participant_id}/mute
POST /v1/participants/{participant_id}/unmute
POST /v1/participants/{participant_id}/drop
POST /v1/rooms/{room_id}/media/attach
POST /v1/rooms/{room_id}/media/publish
POST /v1/rooms/{room_id}/media/unpublish
POST /v1/rooms/{room_id}/media/subscribe
POST /v1/rooms/{room_id}/media/unsubscribe
POST /v1/rooms/{room_id}/media/failover
GET  /v1/calls/{call_id}
GET  /v1/bridges/{bridge_id}
GET  /v1/health
```

`/v1/calls/originate` accepts `call_id`, `bridge_id`, `participant_id`, and `destination`, and returns `pbx_member_id`. State callbacks must include the same `call_id`, the SIP dialog identifiers when known, and one of `RINGING`, `ANSWERED`, `IN_BRIDGE`, `MUTED`, `ENDED`, or `FAILED`.

The engine must negotiate RTP/AVP and support PCMU/8000 and PCMA/8000. It should allocate RTP ports only from the configured range and should not expose its control port outside the private interface.

## Room media semantics

The default room mode is moderated broadcast. Listener legs are muted at the
media engine, but DTMF detection remains enabled. The root mixer receives only
the host and approved speaker publications. Leaf nodes subscribe to the room's
common broadcast and fan it out locally; active speakers may additionally
receive a root-generated mix-minus stream.

Every room media command must include `workspace_id`, `room_id`,
`room_epoch`, `node_epoch`, and an idempotent `command_id`. An engine must
reject commands from an older room or node epoch. Root failover is a media
operation; it must not require re-dialling listeners when only the root mixer
has failed.
