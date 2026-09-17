# Phonarch RustPBX Adapter

This component contains the generic RustPBX deployment configuration and the
fallback heartbeat utility. The production RustPBX media engine is supplied
separately; its integration boundary is documented in
[`/data/docs/RUSTPBX-ADAPTER-CONTRACT.md`](/data/docs/RUSTPBX-ADAPTER-CONTRACT.md).

The template uses private node IPs for SIP/control and a configured public
media IP for RTP/SDP advertisement. SipGo peers are discovered dynamically
from Redis by the sidecar.
