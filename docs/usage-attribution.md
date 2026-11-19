# Usage by project and account

Open `/usage.html` and go to **Activity → Usage breakdown**. Select **Project**, **Account**, or **Project + account** to see requests, failed requests, tokens, and estimated API cost for the selected time range. The account is the upstream credential that handled the request. Provider quota windows above Activity are still shared by every project that uses the same account.

## Assign project names

Give each project a separate, randomly generated proxy API key. In the updated management panel, name the keys under **Config → Access & Authentication**, then save and confirm the configuration. Names previously saved in the same browser are included in the save preview. The panel computes fingerprints automatically.

If you edit YAML directly, keys go under `access.api-keys`. To calculate a fingerprint without putting the key in shell history, run this in Bash:

```bash
read -rs -p 'Proxy API key: ' proxy_key
printf '\n'
printf '%s' "$proxy_key" | sha256sum
unset proxy_key
```

Put the 64-character lowercase digest, without the trailing `-`, in `config.yaml`:

```yaml
observability:
  usage:
    history:
      projects:
        "<64-character digest>": "my-project"
```

Each key can belong to one project. For rotation, map the old and new key fingerprints to the same project name. SQLite stores the name, not the key or fingerprint. Changing a project name applies to new usage only. Requests with no matching mapping and history recorded before this feature appear as **Unassigned**. Imported local transcripts have no proxy account and appear under **Unknown account**. Available account labels come from current quota data; otherwise the dashboard shows the recorded account ID.

History must be enabled. This feature does not require the in-memory usage queue. On first open, the new binary migrates both rollup tables in a single transaction. Stop the old server and back up the SQLite history database before opening it with the new server or importer. Restore that backup before rolling back to the previous binary. Usage written after the backup won't be in the restored database.

API clients can request the same totals with `breakdown=true` on the v8 usage `history` or `stream` endpoint.
