-- M5: real worker harness + attestation + roster.
--
-- Attestation handshake (DESIGN §7.2): a worker submits arch/os; the registry
-- attests arch:*/os:* claims against THESE values (the arch-lottery fix — a
-- worker cannot claim arch:arm64 from an x86 box). role:*/model_family:*/tool:*
-- are attested against the enrolled-identity allowlist (§9.4.1). Only attested
-- caps gate scheduler matching, so an unattested capability is never matched.
ALTER TABLE jobs ADD COLUMN head_ref TEXT;   -- the epoch ref Flowbee promoted onto a branch (M5/M7)

ALTER TABLE workers ADD COLUMN arch TEXT NOT NULL DEFAULT '';
ALTER TABLE workers ADD COLUMN os   TEXT NOT NULL DEFAULT '';
