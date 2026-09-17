# Host configuration

This directory is for versioned, non-secret host topology files rendered or
consumed by deployment tooling. Private credentials belong under
`/data/state/secrets` and runtime data belongs under `/data/state/runtime`.

The current lab environment is supplied through the deployment template at
`/data/deploy/phonarch-conference.env.example`; the active local copy is kept
outside Git in `/data/state/secrets/phonarch-conference.env`.
