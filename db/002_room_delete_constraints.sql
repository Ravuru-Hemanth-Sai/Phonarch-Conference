-- Keep batch and command history deletable when their room-owned records go away.
-- The control API also performs explicit child cleanup for compatibility with
-- databases created before this migration was introduced.
ALTER TABLE IF EXISTS dial_batch_items
  DROP CONSTRAINT IF EXISTS dial_batch_items_participant_id_fkey;

ALTER TABLE IF EXISTS dial_batch_items
  ADD CONSTRAINT dial_batch_items_participant_id_fkey
  FOREIGN KEY (participant_id) REFERENCES participants(id) ON DELETE SET NULL;

ALTER TABLE IF EXISTS node_commands
  DROP CONSTRAINT IF EXISTS node_commands_call_leg_id_fkey;

ALTER TABLE IF EXISTS node_commands
  ADD CONSTRAINT node_commands_call_leg_id_fkey
  FOREIGN KEY (call_leg_id) REFERENCES call_legs(id) ON DELETE SET NULL;
