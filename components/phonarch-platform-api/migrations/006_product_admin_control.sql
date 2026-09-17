-- Product-operator control plane. Customer users are created by product admins;
-- there is intentionally no public self-signup path.

ALTER TABLE operators ADD COLUMN IF NOT EXISTS display_name TEXT NOT NULL DEFAULT '';
ALTER TABLE operators ADD COLUMN IF NOT EXISTS platform_role TEXT NOT NULL DEFAULT 'WORKSPACE_OPERATOR';
ALTER TABLE operators ADD COLUMN IF NOT EXISTS status TEXT NOT NULL DEFAULT 'ACTIVE';

ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS max_participants INTEGER NOT NULL DEFAULT 500;
ALTER TABLE workspaces DROP CONSTRAINT IF EXISTS workspaces_max_participants_check;
ALTER TABLE workspaces ADD CONSTRAINT workspaces_max_participants_check
  CHECK (max_participants BETWEEN 1 AND 100000);

CREATE TABLE IF NOT EXISTS telephony_numbers (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE RESTRICT,
  e164_number TEXT NOT NULL,
  label TEXT NOT NULL DEFAULT '',
  provider_ref TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'ACTIVE',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (workspace_id, e164_number)
);

ALTER TABLE bridges ADD COLUMN IF NOT EXISTS participant_limit INTEGER NOT NULL DEFAULT 500;
ALTER TABLE bridges ADD COLUMN IF NOT EXISTS tfn_id UUID;

UPDATE bridges b
SET participant_limit = w.max_participants
FROM workspaces w
WHERE b.workspace_id = w.id AND (b.participant_limit IS NULL OR b.participant_limit = 500);

DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'bridges_tfn_id_fkey') THEN
    ALTER TABLE bridges ADD CONSTRAINT bridges_tfn_id_fkey
      FOREIGN KEY (tfn_id) REFERENCES telephony_numbers(id) ON DELETE SET NULL;
  END IF;
END $$;

ALTER TABLE bridges DROP CONSTRAINT IF EXISTS bridges_participant_limit_check;
ALTER TABLE bridges ADD CONSTRAINT bridges_participant_limit_check
  CHECK (participant_limit BETWEEN 1 AND 100000);

CREATE UNIQUE INDEX IF NOT EXISTS bridges_one_room_per_tfn_uq
  ON bridges (tfn_id) WHERE tfn_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS telephony_numbers_workspace_idx
  ON telephony_numbers (workspace_id, status, created_at DESC);

ALTER TABLE workspace_members DROP CONSTRAINT IF EXISTS workspace_members_role_check;
ALTER TABLE workspace_members ADD CONSTRAINT workspace_members_role_check
  CHECK (role IN ('OWNER', 'ADMIN', 'OPERATOR', 'VIEWER'));
