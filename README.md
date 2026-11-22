# sbx-broker

sbx-broker is a small Go binary that runs on the host. Apps in Docker sandboxes send placeholders such as `sbxb_<ns>_<name>` as credentials, and the broker swaps in the real values, which it fetches from a command you configure. See `docs/specs/2026-11-22-sbx-broker-design.md` for the design.
