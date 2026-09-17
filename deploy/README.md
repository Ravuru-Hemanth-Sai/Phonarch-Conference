# Deployment layout

The scripts in this directory are the native host entry points:

- `build.sh` builds each component into its component-local `bin/` directory;
- `init-db.sh` applies the platform API migrations;
- `start.sh`, `stop.sh`, and `status.sh` manage the local native profile.

The deployment environment template is
[`phonarch-conference.env.example`](phonarch-conference.env.example). Copy a
private deployment file to `/data/state/secrets/phonarch-conference.env`.
