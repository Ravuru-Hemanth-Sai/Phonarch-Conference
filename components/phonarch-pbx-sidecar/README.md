# Phonarch PBX Sidecar

One native Go sidecar runs beside each RustPBX process. It discovers live
SipGo edges through Redis, sends load-bearing SIP OPTIONS heartbeats, exposes
the local command API, and forwards room/call operations to the RustPBX control
endpoint.
