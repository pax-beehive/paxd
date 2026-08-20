---
name: paxd-install
description: Install paxd from the hosted installer link, download the matching stable binary for the local platform, run interactive Pax pairing, and verify paxd is connected without exposing secrets.
---

# Paxd Install

Use this skill when a user asks an agent to install Pax/paxd on the current machine, connect an agent host to Pax, run the Pax installer link, or complete paxd pairing.

## Install

Run the hosted installer:

```bash
curl -fsSL --max-redirs 1 https://api.lakeward.net/api/v1/public/paxd/install.sh | bash
```

For a self-hosted manager, keep installer and binary resolution on the same
manager. Current release artifacts have this base URL baked in, while the
explicit environment override also works with older installers:

```bash
export PAX_MANAGER_URL='https://pax.home.example'
curl -fsSL --max-redirs 1 "$PAX_MANAGER_URL/api/v1/public/paxd/install.sh" |
  PAX_DOWNLOAD_URL="$PAX_MANAGER_URL" bash
paxd login --remote default --cloud-url "$PAX_MANAGER_URL"
```

The installer detects the local platform, downloads the newest `stable` paxd
binary through pax-manager's resolver, verifies sha256, and installs it into
PATH.

The entry curl follows at most the manager's single installer redirect. Inside
the downloaded installer, both the manager resolver request and the returned
signed object URL reject every redirect.

Pair the daemon with:

```bash
paxd login --remote default --cloud-url https://api.lakeward.net
```

Use one manager base URL consistently for installer, binary download, and paxd
API calls. For hosted Pax that URL is `api.lakeward.net`. The verification URL
printed by paxd is returned by pax-manager and uses the human-facing
`https://ws.lakeward.net` host for hosted Pax.

## Pairing

When `paxd login` prints a verification URL and 6-character code:

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
- If pairing expires, rerun `paxd login --remote default --cloud-url https://api.lakeward.net`.
- If the machine is behind a restricted network, verify it can reach the manager API, the browser pairing host, and the object-storage host returned by the download resolver.
- The public installer and resolver routes must not redirect unattended clients to an interactive Cloudflare Access login. Use an Access bypass/service-auth policy for `/api/v1/public/*` or provide an equivalent machine-readable edge configuration.
