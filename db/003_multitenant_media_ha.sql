-- PhonArch Conference tenancy and room-media HA primitives.
-- This migration is additive: existing bridges are assigned to the seeded
-- Operations workspace and existing call ownership remains intact.

CREATE TABLE IF NOT EXISTS workspaces (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  slug TEXT NOT NULL UNIQUE,
  name TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'ACTIVE',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO workspaces (slug, name)
VALUES ('operations', 'Operations')
ON CONFLICT (slug) DO NOTHING;

CREATE TABLE IF NOT EXISTS workspace_members (
  workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  operator_id UUID NOT NULL REFERENCES operators(id) ON DELETE CASCADE,
  role TEXT NOT NULL DEFAULT 'ADMIN',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, operator_id)
);

ALTER TABLE bridges ADD COLUMN IF NOT EXISTS workspace_id UUID;
UPDATE bridges
SET workspace_id = (SELECT id FROM workspaces WHERE slug = 'operations')
WHERE workspace_id IS NULL;
ALTER TABLE bridges ALTER COLUMN workspace_id SET NOT NULL;

DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'bridges_workspace_id_fkey') THEN
    ALTER TABLE bridges ADD CONSTRAINT bridges_workspace_id_fkey
      FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON DELETE RESTRICT;
  END IF;
END $$;

CREATE UNIQUE INDEX IF NOT EXISTS bridges_workspace_name_uq
  ON bridges (workspace_id, lower(name));

ALTER TABLE participants ADD COLUMN IF NOT EXISTS workspace_id UUID;
UPDATE participants p
SET workspace_id = b.workspace_id
FROM bridges b
WHERE p.bridge_id = b.id AND p.workspace_id IS NULL;
ALTER TABLE participants ALTER COLUMN workspace_id SET NOT NULL;

ALTER TABLE call_legs ADD COLUMN IF NOT EXISTS workspace_id UUID;
UPDATE call_legs c
SET workspace_id = b.workspace_id
FROM bridges b
WHERE c.bridge_id = b.id AND c.workspace_id IS NULL;
ALTER TABLE call_legs ALTER COLUMN workspace_id SET NOT NULL;

ALTER TABLE dial_batches ADD COLUMN IF NOT EXISTS workspace_id UUID;
UPDATE dial_batches d
SET workspace_id = b.workspace_id
FROM bridges b
WHERE d.bridge_id = b.id AND d.workspace_id IS NULL;
ALTER TABLE dial_batches ALTER COLUMN workspace_id SET NOT NULL;

ALTER TABLE dial_batch_items ADD COLUMN IF NOT EXISTS workspace_id UUID;
UPDATE dial_batch_items i
SET workspace_id = d.workspace_id
FROM dial_batches d
WHERE i.batch_id = d.id AND i.workspace_id IS NULL;
ALTER TABLE dial_batch_items ALTER COLUMN workspace_id SET NOT NULL;

ALTER TABLE node_commands ADD COLUMN IF NOT EXISTS workspace_id UUID;
ALTER TABLE node_commands ADD COLUMN IF NOT EXISTS bridge_id UUID;
ALTER TABLE node_commands ADD COLUMN IF NOT EXISTS room_epoch BIGINT NOT NULL DEFAULT 1;
ALTER TABLE node_commands ADD COLUMN IF NOT EXISTS command_sequence BIGINT NOT NULL DEFAULT 0;
UPDATE node_commands n
SET workspace_id = b.workspace_id,
    bridge_id = c.bridge_id
FROM call_legs c
JOIN bridges b ON b.id = c.bridge_id
WHERE n.call_leg_id = c.id AND n.workspace_id IS NULL;

DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'participants_workspace_id_fkey') THEN
    ALTER TABLE participants ADD CONSTRAINT participants_workspace_id_fkey
      FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON DELETE RESTRICT;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'call_legs_workspace_id_fkey') THEN
    ALTER TABLE call_legs ADD CONSTRAINT call_legs_workspace_id_fkey
      FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON DELETE RESTRICT;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'dial_batches_workspace_id_fkey') THEN
    ALTER TABLE dial_batches ADD CONSTRAINT dial_batches_workspace_id_fkey
      FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON DELETE RESTRICT;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'dial_batch_items_workspace_id_fkey') THEN
    ALTER TABLE dial_batch_items ADD CONSTRAINT dial_batch_items_workspace_id_fkey
      FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON DELETE RESTRICT;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'node_commands_workspace_id_fkey') THEN
    ALTER TABLE node_commands ADD CONSTRAINT node_commands_workspace_id_fkey
      FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON DELETE RESTRICT;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'node_commands_bridge_id_fkey') THEN
    ALTER TABLE node_commands ADD CONSTRAINT node_commands_bridge_id_fkey
      FOREIGN KEY (bridge_id) REFERENCES bridges(id) ON DELETE CASCADE;
  END IF;
END $$;

CREATE INDEX IF NOT EXISTS participants_workspace_bridge_idx
  ON participants (workspace_id, bridge_id, created_at);
CREATE INDEX IF NOT EXISTS call_legs_workspace_owner_idx
  ON call_legs (workspace_id, node_id, state);
CREATE INDEX IF NOT EXISTS dial_batches_workspace_idx
  ON dial_batches (workspace_id, bridge_id, status);

CREATE TABLE IF NOT EXISTS room_media_sessions (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE RESTRICT,
  bridge_id UUID NOT NULL UNIQUE REFERENCES bridges(id) ON DELETE CASCADE,
  room_epoch BIGINT NOT NULL DEFAULT 1,
  active_root_node_id TEXT,
  standby_root_node_id TEXT,
  status TEXT NOT NULL DEFAULT 'READY',
  broadcast_ssrc BIGINT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS room_leaf_assignments (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE RESTRICT,
  bridge_id UUID NOT NULL REFERENCES bridges(id) ON DELETE CASCADE,
  node_id TEXT NOT NULL,
  node_epoch BIGINT NOT NULL DEFAULT 1,
  stream_id TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'ACTIVE',
  last_seen_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (bridge_id, node_id)
);

CREATE TABLE IF NOT EXISTS speaker_requests (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE RESTRICT,
  bridge_id UUID NOT NULL REFERENCES bridges(id) ON DELETE CASCADE,
  participant_id UUID NOT NULL REFERENCES participants(id) ON DELETE CASCADE,
  call_leg_id UUID REFERENCES call_legs(id) ON DELETE SET NULL,
  digit TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'QUEUED',
  requested_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  granted_at TIMESTAMPTZ,
  withdrawn_at TIMESTAMPTZ,
  granted_by TEXT,
  command_sequence BIGINT NOT NULL DEFAULT 0
);

CREATE UNIQUE INDEX IF NOT EXISTS speaker_requests_open_uq
  ON speaker_requests (bridge_id, participant_id)
  WHERE status IN ('QUEUED', 'GRANTED', 'ACTIVE');
CREATE INDEX IF NOT EXISTS speaker_requests_room_queue_idx
  ON speaker_requests (workspace_id, bridge_id, status, requested_at);

CREATE TABLE IF NOT EXISTS room_events (
  id BIGSERIAL PRIMARY KEY,
  workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE RESTRICT,
  bridge_id UUID NOT NULL REFERENCES bridges(id) ON DELETE CASCADE,
  event_type TEXT NOT NULL,
  participant_id UUID REFERENCES participants(id) ON DELETE SET NULL,
  room_epoch BIGINT NOT NULL DEFAULT 1,
  payload JSONB NOT NULL DEFAULT '{}',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO room_media_sessions (workspace_id, bridge_id)
SELECT b.workspace_id, b.id
FROM bridges b
ON CONFLICT (bridge_id) DO NOTHING;

INSERT INTO workspace_members (workspace_id, operator_id)
SELECT w.id, o.id
FROM workspaces w
JOIN operators o ON o.username = 'admin'
WHERE w.slug = 'operations'
ON CONFLICT DO NOTHING;
