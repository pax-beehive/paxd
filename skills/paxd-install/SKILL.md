---
name: paxd-install
description: Install paxd from the hosted installer link, download the matching stable binary for the local platform, run interactive Pax pairing, and verify paxd is connected without exposing secrets.
---

# Paxd Install

Use this skill when a user asks an agent to install Pax/paxd on the current machine, connect an agent host to Pax, run the Pax installer link, or complete paxd pairing.

## Install

Run the hosted installer:

```bash
curl -fsSL https://api.paxtech.net/api/v1/public/paxd/install.sh | bash
```

The installer detects the local platform, downloads the newest `stable` paxd binary, verifies sha256, installs it into PATH, then runs:

```bash
paxd connect --cloud-url https://ws.paxtech.net
```

Use `api.paxtech.net` for installer and binary download endpoints. Use `ws.paxtech.net` as the human-facing Pax entry for login and pairing.

## Pairing

When `paxd connect` prints a verification URL and 6-character code:

1. Show the URL and code clearly to the user.
2. Ask the user to open the URL, log in, and approve pairing.
3. Wait for paxd to report success.

Do not ask the user to paste API keys. Do not print local config contents after pairing. The node API key is written by paxd to the local config file.

## Verify

After the installer finishes, run:

```bash
paxd --version
test -f ~/.paxd/paxd.yaml
```

If the user wants paxd running immediately, run:

```bash
paxd run
```

## Troubleshooting

- If `curl` or `python3` is missing, install it with the system package manager and rerun the installer.
- If the installer cannot write to the selected PATH directory, rerun with `PAX_INSTALL_DIR=$HOME/.local/bin` and ensure that directory is on PATH.
- If pairing expires, rerun `paxd connect --cloud-url https://ws.paxtech.net`.
- If the machine is behind a restricted network, verify it can reach `https://api.paxtech.net`, `https://ws.paxtech.net`, and Google Cloud Storage.
