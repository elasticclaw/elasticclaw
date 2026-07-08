# Secrets at rest in hub.yaml

The hub encrypts sensitive fields in `hub.yaml` (tokens, passwords, LLM API
keys, GitHub App private keys, integration tokens, webhook secrets) using
AES-256-GCM. Encrypted values look like:

```yaml
token: enc:v1:xkQ2rn3...   # base64(nonce || ciphertext)
```

Non-sensitive fields stay plaintext, so `hub.yaml` remains readable and
editable by hand.

## Master key

The 32-byte master key is resolved in this order:

1. `ELASTICCLAW_MASTER_KEY` environment variable (base64-encoded 32 bytes).
   The installer generates this and injects it into the systemd unit via a
   0600 `EnvironmentFile` at `/etc/elasticclaw/master.env`.
2. A `master.key` file, created automatically on first hub boot with 0600
   permissions:
   - next to the hub config when `ELASTICCLAW_HUB_CONFIG` points at a custom
     path,
   - `~/.elasticclaw/master.key` otherwise.

When running CLI commands (e.g. `elasticclaw hub encrypt-secrets`) on a
server installed with the systemd unit, source the key first so the CLI uses
the same key as the service:

```sh
set -a; . /etc/elasticclaw/master.env; set +a
```

## Migration

- Plaintext values (configs written before encryption existed, or edited by
  hand) are accepted on load and rewritten encrypted the next time the hub
  saves its config. No manual intervention is required on upgrade.
- To force the full migration immediately:

  ```sh
  elasticclaw hub encrypt-secrets
  ```

## Runbook: lost master key

Encrypted values cannot be recovered without the master key. If the key is
lost (deleted `master.env`/`master.key` and no backup):

1. Stop the hub.
2. Edit `hub.yaml` and replace every `enc:v1:...` value with its plaintext
   secret (re-issue tokens/keys you no longer have: GitHub App private key,
   Linear/Shortcut/Jira tokens, LLM API keys, etc.).
3. Delete the stale key so a fresh one is generated:
   `rm -f ~/.elasticclaw/master.key /etc/elasticclaw/master.env` (adjust
   paths to your install; recreate `master.env` if your systemd unit expects
   it, using `ELASTICCLAW_MASTER_KEY=$(openssl rand -base64 32)`).
4. Start the hub. Plaintext values load fine and are re-encrypted under the
   new key on the next save, or run `elasticclaw hub encrypt-secrets`.

Back up the master key (`/etc/elasticclaw/master.env` or
`~/.elasticclaw/master.key`) separately from `hub.yaml`.
