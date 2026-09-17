# Phonarch SIP Gateway

Go service built with `github.com/emiago/sipgo`. It is the public SIP
UDP/TCP edge, Redis-backed PBX selector, dialog-affinity store, OPTIONS
heartbeat receiver, and DTMF event boundary.

The service does not own customer configuration. The platform API and Redis
leases provide the dynamic node and dialog state.
