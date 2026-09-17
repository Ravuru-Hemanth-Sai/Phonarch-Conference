-- Fixed room roster and explicit conference-session lifecycle.

ALTER TABLE bridges ADD COLUMN IF NOT EXISTS room_state TEXT NOT NULL DEFAULT 'READY';
ALTER TABLE bridges ADD COLUMN IF NOT EXISTS host_name TEXT;
ALTER TABLE bridges ADD COLUMN IF NOT EXISTS host_phone TEXT;

UPDATE bridges
SET room_state = 'READY'
WHERE room_state IS NULL OR room_state IN ('ACTIVE', 'INACTIVE');

ALTER TABLE participants ADD COLUMN IF NOT EXISTS role TEXT NOT NULL DEFAULT 'LISTENER';
UPDATE participants SET role = 'LISTENER' WHERE role IS NULL OR role = '';
ALTER TABLE participants ALTER COLUMN desired_state SET DEFAULT 'MUTED';

CREATE UNIQUE INDEX IF NOT EXISTS participants_one_host_per_room_uq
  ON participants (bridge_id) WHERE role = 'HOST';

CREATE TABLE IF NOT EXISTS room_sessions (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE RESTRICT,
  bridge_id UUID NOT NULL REFERENCES bridges(id) ON DELETE CASCADE,
  host_participant_id UUID REFERENCES participants(id) ON DELETE SET NULL,
  room_epoch BIGINT NOT NULL DEFAULT 1,
  status TEXT NOT NULL DEFAULT 'STARTING',
  started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  ended_at TIMESTAMPTZ,
  duration_seconds BIGINT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS room_sessions_one_active_uq
  ON room_sessions (bridge_id) WHERE status IN ('STARTING', 'RUNNING');
CREATE INDEX IF NOT EXISTS room_sessions_room_history_idx
  ON room_sessions (workspace_id, bridge_id, started_at DESC);

CREATE TABLE IF NOT EXISTS room_session_participants (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  session_id UUID NOT NULL REFERENCES room_sessions(id) ON DELETE CASCADE,
  participant_id UUID REFERENCES participants(id) ON DELETE SET NULL,
  call_leg_id UUID REFERENCES call_legs(id) ON DELETE SET NULL,
  display_name TEXT NOT NULL,
  phone_number TEXT NOT NULL,
  role TEXT NOT NULL DEFAULT 'LISTENER',
  call_state TEXT NOT NULL DEFAULT 'DISPATCHED',
  joined_at TIMESTAMPTZ,
  left_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS room_session_participants_history_idx
  ON room_session_participants (session_id, created_at);
