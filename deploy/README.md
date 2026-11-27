# VPS Deploy

## Infrastructure as Code (Terraform + Ansible)

The `terraform/` and `ansible/` subdirectories provision and configure the VPS from scratch. Run these once before the CI/CD pipeline takes over.

### Automated provisioning (recommended)

A single script runs all three steps (Terraform, Ansible, GitHub secrets) inside a Docker container — no host-side Terraform or Ansible required.

**Prerequisites:**

- Docker
- A Hetzner Cloud API token (read/write) from [console.hetzner.cloud](https://console.hetzner.cloud)
- An SSH keypair for the deploy user
- `GH_PAT` set in the root `.env` file (used by Ansible to clone private submodules and by the secrets upload step)

```bash
# Fill in your Hetzner token and SSH public key
cp deploy/provision/terraform/terraform.tfvars.example deploy/provision/terraform/terraform.tfvars
$EDITOR deploy/provision/terraform/terraform.tfvars

# Production is the default. Use --edge-mode development to skip GoDaddy DNS
# and Let's Encrypt while retaining the self-signed TLS path.
deploy/provision/run.sh [--key ~/.ssh/your_key] [--edge-mode production|development]
```

This creates a **cx33** server (4 vCPU, 8 GB RAM, 80 GB disk, Debian 12) in `hel1` (Helsinki), configures it with Docker CE and the `deploy` user, clones a thin parent-repository checkout to `/opt/dexArbTrading`, and writes all 11 GitHub repository secrets. The next push to `main` deploys the full compose stack automatically — including auto-generating nginx's self-signed TLS cert on the VPS (no manual cert step; see [monitoring/README.md](monitoring/README.md#access)).

The script is idempotent — safe to re-run if it fails partway through.

### Production DNS and HTTPS (GoDaddy)

This production flow is additive: the existing IP/self-signed TLS setup remains
available for development servers. Choose the desired mode when provisioning:

```bash
# Default: GoDaddy DNS, public hostname, and Let's Encrypt renewal.
deploy/provision/run.sh --edge-mode production

# No GoDaddy DNS or Let's Encrypt; use the VPS IP with the self-signed cert.
deploy/provision/run.sh --edge-mode development
```

Production uses the `PUBLIC_HOSTNAME` apex record, public ports 80/443, and a
Let's Encrypt HTTP-01 certificate. Development does not require `GODADDY_PAT`,
does not create GoDaddy records, and does not install the certificate-renewal
timer. Both modes retain the self-signed certificate generated during deploy so
nginx can start before a trusted certificate exists.

Set these root `.env` values locally before provisioning. `.env` is ignored;
the encrypted `.env.enc` is committed by the post-commit hook.

```dotenv
GODADDY_PAT=<PAT with domains.dns:update>
PUBLIC_HOSTNAME=dexmonitor.live
LETSENCRYPT_EMAIL=info@dexmonitor.live
```

In production mode, the provisioning job reads the Terraform-managed VPS
address, passes the hostname and PAT to Ansible over SSH, and reconciles the
GoDaddy apex (`@`) A record. It then waits until the VPS resolves that hostname
to its own address. Terraform exposes 80 for the ACME HTTP-01 challenge and
HTTP-to-HTTPS redirect, and 443 for the public dashboard, Telegram webhook, and the two
protected phone-payment callback paths: `/pushpush/notify` and
`/pushpush/phone-heartbeat`. nginx does not expose any other Fiat Controller route.
Set `PUSHPUSH_NOTIFY_SECRET` in the deployment secret environment before enabling a
phone; both public callback paths reject all requests when it is absent. The same
secret authorizes notification and heartbeat requests.
The authenticated Communication Hub reveals these deployment-managed values only on a
deliberate request and its QR codes never include them.
The initial CI rollout issues the certificate; Ansible also installs a daily
renewal timer.

To reconcile DNS later without creating or modifying Terraform resources,
deploying the application, or writing GitHub secrets, run this command from a
machine with Docker and the provisioning SSH key:

```bash
deploy/provision/run.sh --dns-only
```

The command still reads Terraform state to obtain the existing VPS IP, then
connects to the VPS itself; there is no need to log in manually. It requires
`GODADDY_PAT`, `PUBLIC_HOSTNAME`, Terraform state, and the same SSH key as the
full provisioning job. It updates only the apex A record, with a 600-second
TTL. `--dns-only` is intentionally unavailable in development mode.

> **GoDaddy Website Builder.** A protected `WebsiteBuilder Site` parking record
> cannot be deleted through the DNS API. Detach or remove that Website Builder
> site in the GoDaddy dashboard first, then rerun `--dns-only`. Do not create a
> second apex A record while the parking record remains: requests could be
> served by either destination.

### Teardown

```bash
cd deploy/provision/terraform
terraform destroy
```

### Regenerating DBeaver connection files

The VPS IP changes whenever Terraform recreates the box. `deploy/provision/dbeaver-sync.sh` generates a DBeaver project archive (`dexarb-vps.dbp`, with the connection details and an AES-encrypted password bundled) from the current `deploy/provision/.env.vps` and the root `.env`. Output lands in `deploy/provision/dbeaver-out/` (gitignored).

```bash
deploy/provision/dbeaver-sync.sh
```

Import it via DBeaver: **File → Import → DBeaver → Project**, then select `deploy/provision/dbeaver-out/dexarb-vps.dbp`. It imports as a separate project (`dexPendulum-vps-import`) with the password bundled. The import always creates a new project, so to re-import after an IP change, delete the existing `dexPendulum-vps-import` project first.

The script also writes `deploy/provision/dbeaver-out/tabularis-dexarb-vps.json` — the same connection as a Tabularis import payload. Import via Tabularis: **Connections page → Import dropup → Tabularis JSON**. Re-importing merges by id, so regenerating and re-importing after an IP change updates the connection in place (no duplicates). Note: like Tabularis' own exports, this JSON contains the DB password in plaintext; it lives in the gitignored output directory with 600 perms.

---

### Manual steps (alternative)

If you prefer to run the steps individually, install [Terraform](https://developer.hashicorp.com/terraform/install) ≥ 1.5 and [Ansible](https://docs.ansible.com/ansible/latest/installation_guide/) ≥ 2.14 on your host, then follow the steps below.

### 1 — Provision with Terraform

```bash
cd deploy/provision/terraform
cp terraform.tfvars.example terraform.tfvars   # fill in token + SSH public key
terraform init
terraform apply
```

This creates a **cx33** server (4 vCPU, 8 GB RAM, Debian 12) in `hel1` (Helsinki, cheapest region), a firewall (opens 22, receive-only SMTP 25, and public 80/443; Postgres/Grafana/Prometheus only to `trusted_cidrs`), and uploads your SSH key. Takes ~30 seconds.

```bash
# Capture the IP for the next step
SERVER_IP=$(terraform output -raw server_ip)
```

### 2 — Configure with Ansible

```bash
cd deploy/provision/ansible
sed "s/SERVER_IP/$SERVER_IP/" inventory.ini.tpl > inventory.ini
ansible-playbook -i inventory.ini playbook.yml \
  --private-key ~/.ssh/your_key
```

Ansible installs Docker CE, adds the `deploy` user to the `docker` group, configures log rotation, and clones only the parent production repo to `/opt/dexArbTrading`. Service source remains in CI; the VPS pulls pre-built images and keeps submodule worktrees deinitialized.

### 3 — Wire up CI/CD

After Ansible completes, add or update these GitHub repository secrets (see [GitHub Actions Wiring](#github-actions-wiring) below):

| Secret | Value |
|--------|-------|
| `VPS_SSH_HOST` | value of `$SERVER_IP` |
| `VPS_SSH_USER` | `deploy` |
| `VPS_SSH_PRIVATE_KEY` | private half of the keypair you used above |
| `VPS_SSH_KNOWN_HOSTS` | output of `ssh-keyscan $SERVER_IP` (all key types) |
| `VPS_REPO_DIR` | `/opt/dexArbTrading` |

> **Note:** `VPS_SSH_KNOWN_HOSTS` must include all three host key types (RSA, ECDSA, ED25519) from `ssh-keyscan`. A single-type scan will cause "Host key verification failed" in CI because OpenSSH negotiates ED25519 by default.

The next push to `main` will deploy the full compose stack automatically.

---

This folder contains secrets bootstrap scripts for the CI/CD pipeline.

## What It Does

Tests and image builds run in GitHub Actions ([`.github/workflows/ci-cd.yml`](../.github/workflows/ci-cd.yml)). The VPS only pulls pre-built images from GHCR and restarts the stack:

1. The CI/CD workflow finishes a successful build and pushes images to `ghcr.io/<owner>/dexarbtrading-<service>:latest` (and `:<sha>`).
2. The `pre-deploy` job verifies every image SHA tag is present in GHCR, then SSHes into the VPS,
   prunes unused images with [`prune-images.sh`](prune-images.sh), and confirms docker, compose,
   and disk (at least 5 GB free) are healthy.
3. The `deploy` job stages decrypted env files, pushes the release commit into
   the VPS bare-ish checkout, then activates via [`ci-activate.sh`](ci-activate.sh):
   the transactional deploy runs **detached** on the VPS and CI polls
   `.deploy/runs/<sha>/result` over short SSH sessions (with keepalives). This
   avoids false failures when a long-lived SSH pipe hangs after a successful
   rollout. Run artifacts: `.deploy/runs/<sha>/{pid,deploy.log,result}`.
4. Post-activate cleanup removes staging only when that SHA is not still running.

`prune-images.sh` deletes every image except those used by a container (running or stopped)
and those the retained releases under `.deploy/releases` reference: the active and previous
release, so exact rollback keeps working offline. `docker image prune -a` would delete the
previous release's images. A healthy deploy runs it again after trimming old releases, and
it skips while another deployment holds `.deploy/deploy.lock`. Preview it on the VPS with
`bash deploy/prune-images.sh --repo-dir /opt/dexArbTrading --dry-run`.

Activation starts Postgres, runs `unified-api ./server migrate` from the pinned
candidate image with the staged environment, then starts the app services. A
migration failure aborts candidate app startup. Rollback restores the prior
release without applying schema migrations; schema changes must therefore be
compatible with the previous release (see the expand/contract rule and
`TestNewMigrationsDeclareRollbackCompatibility` in `dexUnifiedApi/README.md`). App startup retains role/password
reconciliation, but only checks schema history.

The migrate step is bounded while the previous release keeps serving:

- It runs only on images labelled `dexarbtrading.unified-api.migrate=1`. An
  older binary would ignore the argument and start a second API server, so an
  unlabelled image (usually a stale `dexUnifiedApi` submodule pointer) fails
  activation immediately.
- The one-off `dexarb-migrate-<sha>` container is stopped and removed after
  `DEPLOY_MIGRATION_TIMEOUT_SEC` (default 900).
- Each migration waits at most 5 seconds for a table lock and retries up to 10
  times, so a long application transaction cannot queue live queries behind a
  pending exclusive lock.
- The migrate step applies the candidate's database role passwords. If
  activation then fails and those passwords differ from the previous release,
  rollback starts Postgres and recreates `unified-api` with the restored
  environment before waiting for the whole stack. Its startup reapplies the
  previous passwords, allowing unhealthy containers to reconnect.

CI polls the detached deployment for up to 120 minutes. Both GitHub and Gitea
allow 130 minutes for activation and 140 minutes for the deploy job, reserving
time for staging and cleanup. This covers the worst-case critical path —
migration, both 30-minute stack health waits, password restoration — plus
smoke probes and transfer overhead. These are maximum waits; successful
deployments finish immediately. When overriding migration or polling limits,
adjust the enclosing CI limits too.

CI computes image changes against each service's source in the production
baseline. The VPS verifies that baseline is still current and checks build
receipts and digests; it does not recompute source diffs from submodule objects.
Failed activation logs include Docker healthcheck timestamps, exit codes and
output, OOM/exit state, restart counts, and recent application logs for unhealthy,
starting, or stopped containers.

This keeps the VPS thin: no builds, no test runs — just an SSH endpoint for docker compose commands.

To pull private GHCR images, run `docker login ghcr.io` once as the deploy user on the VPS using a read-only PAT (classic token with `read:packages` scope).

## GitHub Actions Wiring

The `pre-deploy` and `deploy` jobs in [`.github/workflows/ci-cd.yml`](../.github/workflows/ci-cd.yml) need these repository secrets:

- `VPS_SSH_HOST`: VPS hostname or IP.
- `VPS_SSH_USER`: the deploy user on the VPS.
- `VPS_SSH_PRIVATE_KEY`: full ed25519 private key for the deploy user. Generate a dedicated keypair (`ssh-keygen -t ed25519 -f deploy_key -N ''`), put the public half in `/home/<user>/.ssh/authorized_keys` on the VPS, and store the private half here.
- `VPS_SSH_KNOWN_HOSTS`: output of `ssh-keyscan -t ed25519 <vps-host>` from a trusted machine. Prevents TOFU prompts and MITM during deploy.
- `VPS_REPO_DIR`: absolute path to the repo checkout on the VPS (e.g. `/opt/dexArbTrading`).

## Secrets Management

Plaintext `.env` files are gitignored everywhere. Instead, each repo carries an encrypted `.env.enc` that is committed and safe to push. The private decryption keys live only in your local `.env.keys` files and in GitHub repository secrets. The CI/CD pipeline decrypts at deploy time and uploads the plaintext only to the VPS over SSH.

### Local setup — Lefthook, dotenvx, and Gitleaks

Every developer machine that commits to this monorepo (or any submodule) needs
three CLI tools installed and the Git hooks wired up. Gitleaks is pinned to
`8.30.1`, matching CI.

**Install tools** (one-time per machine):

```bash
mkdir -p "$HOME/.local/bin"

# dotenvx — encrypts/decrypts .env files
curl -sfS "https://dotenvx.sh?directory=$HOME/.local/bin" | sh

# lefthook — git hook manager
curl -sL "https://github.com/evilmartians/lefthook/releases/latest/download/lefthook_Linux_x86_64" \
  -o "$HOME/.local/bin/lefthook" && chmod +x "$HOME/.local/bin/lefthook"

# Gitleaks 8.30.1 — scans staged content for recognizable secrets (Linux x64)
curl -fsSL "https://github.com/gitleaks/gitleaks/releases/download/v8.30.1/gitleaks_8.30.1_linux_x64.tar.gz" \
  -o /tmp/gitleaks_8.30.1_linux_x64.tar.gz
printf '%s  %s\n' \
  '551f6fc83ea457d62a0d98237cbad105af8d557003051f41f3e7ca7b3f2470eb' \
  '/tmp/gitleaks_8.30.1_linux_x64.tar.gz' | sha256sum -c -
tar -xzf /tmp/gitleaks_8.30.1_linux_x64.tar.gz -C "$HOME/.local/bin" gitleaks
chmod +x "$HOME/.local/bin/gitleaks"
```

**Install hooks** — run once in each repo after cloning:

```bash
# Parent repo
lefthook install

# Each submodule
for repo in dexFiatController dexTakerController dexMonitor dexUnifiedApi; do
  (cd "$repo" && lefthook install)
done
```

The parent repository also installs a `post-checkout` hook. When the local dev
stack is already running, checking out another branch compares the old and new
commits and rebuilds only the affected dev services. It does nothing when the
dev stack is stopped. Re-run `lefthook install` after pulling this hook change.

**Secret protection before every commit:**

Each repository's `pre-commit` hook inspects added, modified, and renamed staged
paths. It rejects plaintext dotenv names (`.env` and every `.env.*` variant)
unless the filename ends in `.enc` or `.example`. For example, `.env.enc`,
`.env.example`, `.env.production.enc`, and `.env.vps.example` are allowed;
`.env`, `.env.local`, and `.env.production` are rejected.

After the filename check, the hook runs
`gitleaks git --pre-commit --staged --redact` with that repository's
`.gitleaks.toml`. Recognizable secrets in any other staged file block the
commit, and values are redacted from output. Encrypted dotenv files and dotenv
templates are excluded from content scanning. The hook fails closed with an
installation command if `gitleaks` is missing.

To remediate a rejected dotenv file, remove it from the index, keep the
plaintext file local, and commit its encrypted `.env.enc` counterpart.

**Encryption after every commit:**

The `post-commit` hook defined in each repo's [`lefthook.yml`](../lefthook.yml) runs `.lefthook/post-commit/encrypt-env.sh` automatically after every commit. It:

1. Skips if the commit message starts with `.env values update` (loop guard).
2. Skips if no `.env` file exists in the repo root.
3. Copies `.env` → `.env.enc` and encrypts the copy in-place with `dotenvx encrypt -f .env.enc`.
4. Stages `.env.enc` and creates a follow-up commit: `.env values update <reponame>`.

The original `.env` is never modified. The private key is written to `.env.keys` (gitignored). Only `.env.enc` is committed.

**Branch-protection guards (also installed by `lefthook install`):**

The same [`lefthook.yml`](../lefthook.yml) wires two guards that keep production safe on dev machines:

- `pre-push` → `.lefthook/pre-push/protect-main.sh` rejects any push whose target ref is `refs/heads/main`.
- `pre-merge-commit` → `.lefthook/pre-merge-commit/protect-main.sh` rejects a local merge made while on `main`.

Local hooks can be bypassed with `--no-verify`. Both GitHub and Gitea CI
therefore run an early security job over the checked-out root and recursive
submodules: the job repeats the strict dotenv filename check and scans the
complete current snapshot with Gitleaks in redacted directory mode. Downstream
CI jobs wait for this backstop to pass.

The branch guards are convenience guards. On this Free private repo they are
the protection for `main`; non-bypassable server-side GitHub branch protection
(PR + green checks) would require GitHub Pro or making the repo public. All work
flows `feature/* → develop → main`; `develop` is the default branch and only a
reviewed `develop → main` PR updates production. See the README's *Branching &
environments* section.

Merged branches are cleaned up automatically: `delete_branch_on_merge` removes a PR's head branch on merge, the weekly [`branch-cleanup.yml`](../.github/workflows/branch-cleanup.yml) workflow sweeps any branch already merged into `develop`/`main` (keeping `main`/`develop` and open-PR branches; run it via *Actions → Branch cleanup* with `dry_run` to preview), and `make prune` clears locally-merged branches.

### Automated secrets upload

Instead of copying keys manually, run the appropriate bootstrap script from the repo root:

```bash
# GitHub
./setup-github-secrets.sh

# Gitea
./setup-gitea-secrets.sh
```

Both scripts read `GH_PAT` from `.env` for authentication and derive the repo slug from `git remote get-url origin`. The GitHub script installs `gh` CLI automatically if missing (Debian/Ubuntu only — install it manually for other distros). The Gitea script uses plain `curl` with no extra dependencies.

### Extracting private keys for GitHub secrets

After the first encryption in each repo, the private key is stored in `.env.keys`. You need to add these to GitHub repository secrets so CI can decrypt at deploy time.

The key name dotenvx uses for a file named `.env.enc` is `DOTENV_PRIVATE_KEY_ENC`. Extract it per repo:

```bash
# Root repo
grep DOTENV_PRIVATE_KEY_ENC .env.keys

# Submodules
grep DOTENV_PRIVATE_KEY_ENC dexUnifiedApi/.env.keys
grep DOTENV_PRIVATE_KEY_ENC dexMonitor/.env.keys
grep DOTENV_PRIVATE_KEY_ENC dexTakerController/.env.keys
grep DOTENV_PRIVATE_KEY_ENC dexFiatController/.env.keys
```

Add each value to **Settings → Secrets and variables → Actions** in the GitHub repo:

| Secret name | Source |
|---|---|
| `DOTENV_PRIVATE_KEY_ROOT` | root `.env.keys` |
| `DOTENV_PRIVATE_KEY_UNIFIED_API` | `dexUnifiedApi/.env.keys` |
| `DOTENV_PRIVATE_KEY_MONITOR` | `dexMonitor/.env.keys` |
| `DOTENV_PRIVATE_KEY_MM_CONTROLLER` | `dexTakerController/.env.keys` |
| `DOTENV_PRIVATE_KEY_FIAT_CONTROLLER` | `dexFiatController/.env.keys` |

Keep `.env.keys` backed up securely (password manager, encrypted vault). If a key is lost the `.env.enc` cannot be decrypted and secrets must be rotated.

### Rotating keys

To generate a fresh keypair and re-encrypt a file:

```bash
# Root repo
dotenvx rotate -f .env.enc

# Submodules
dotenvx rotate -f dexUnifiedApi/.env.enc
dotenvx rotate -f dexMonitor/.env.enc
dotenvx rotate -f dexTakerController/.env.enc
dotenvx rotate -f dexFiatController/.env.enc
```

Each `rotate` call:
1. Generates a new keypair.
2. Re-encrypts the file in-place with the new public key.
3. Prints the new private key.

After rotating, update the corresponding GitHub secret with the newly printed private key and commit the changed `.env.enc` files. The old private key is immediately invalid.

To inspect the current keypair for a file without rotating it:

```bash
dotenvx keypair -f .env.enc
```

### GitHub Actions submodule access

The CI checkout steps that clone submodules require a GitHub Personal Access Token (classic with `repo` scope, or a fine-grained token with `Contents: read` for each submodule repo). Add it as a repository secret:

| Secret name | Value |
|---|---|
| `GH_PAT` | PAT with read access to all submodule repositories |

The token is passed to `actions/checkout` via `token: ${{ secrets.GH_PAT }}` in the `test-go`, `test-frontend`, and `build-and-push` jobs.

### CI/CD decryption flow

The `deploy` job in [`.github/workflows/ci-cd.yml`](../.github/workflows/ci-cd.yml) handles secrets delivery to the VPS without ever storing plaintext in the repo or in CI logs:

1. **Verify** — checks that all five `.env.enc` files are present in the checkout. If any is missing the job fails with a hint to run the encryption locally.
2. **Install dotenvx** on the runner (`curl -sfS https://dotenvx.sh | sh`).
3. **Decrypt** — each `.env.enc` is decrypted in-place using its corresponding private key injected as an environment variable:
   ```
   DOTENV_PRIVATE_KEY_ENC=<key>  dotenvx decrypt -f <path>/.env.enc
   ```
   After this step the files on the runner contain plaintext values.
4. **Upload** — each decrypted file is SCPed to the VPS as `.env` in the correct location under `$REPO_DIR`.
5. **Permissions** — a final SSH command sets `chmod 600` on every uploaded `.env`.

The decrypted files exist only in the runner's ephemeral workspace and are never logged, cached, or uploaded as artifacts.

### Manual env upload (without CI)

If CI is unavailable, decrypt the `.env.enc` files locally and SCP them manually:

```bash
# Decrypt (requires .env.keys in each directory)
dotenvx decrypt -f .env.enc
dotenvx decrypt -f dexUnifiedApi/.env.enc
dotenvx decrypt -f dexMonitor/.env.enc
dotenvx decrypt -f dexTakerController/.env.enc
dotenvx decrypt -f dexFiatController/.env.enc

# Upload
scp .env.enc                  deploy@<vps-host>:/opt/dexArbTrading/.env
scp dexUnifiedApi/.env.enc    deploy@<vps-host>:/opt/dexArbTrading/dexUnifiedApi/.env
scp dexMonitor/.env.enc       deploy@<vps-host>:/opt/dexArbTrading/dexMonitor/.env
scp dexTakerController/.env.enc  deploy@<vps-host>:/opt/dexArbTrading/dexTakerController/.env
scp dexFiatController/.env.enc deploy@<vps-host>:/opt/dexArbTrading/dexFiatController/.env

ssh deploy@<vps-host> "chmod 600 /opt/dexArbTrading/.env /opt/dexArbTrading/{dexUnifiedApi,dexMonitor,dexTakerController,dexFiatController}/.env"
```

## One-time VPS Setup

### Receive-only payment mail

Maddy is pinned to `foxcpp/maddy:0.9.5`. Compose publishes SMTP 25 and keeps TLS
IMAP 993 private. The configuration has no submission endpoint, outbound queue, relay,
alias catch-all, or remote delivery target. Only `MADDY_RECIPIENT` is accepted. SPF,
DKIM, and DMARC checks run on ingress and failures use Maddy's quarantine action.

Before starting the service:

1. Assign the VPS a static public address. Create `A` (and `AAAA` only when IPv6 is
   actually routed) for `MADDY_HOSTNAME`, for example `mx.example.com`.
2. Create an MX record for `MADDY_DOMAIN`: `example.com. MX 10 mx.example.com.` Set the
   address PTR/rDNS to the same Maddy hostname.
3. Obtain a certificate whose SAN contains `MADDY_HOSTNAME`. Install it as
   `deploy/maddy/tls/fullchain.pem` and its key as
   `deploy/maddy/tls/privkey.pem`; restrict the private key to the deploy operator.
4. Set `MADDY_HOSTNAME`, `MADDY_DOMAIN`, `MADDY_RECIPIENT`, and a long random
   `MADDY_IMAP_PASSWORD` in the encrypted root environment.
   Set `COMPOSE_PROFILES=mail` there as well. Without that explicit opt-in,
   Compose omits the mail services so an ordinary deploy cannot start SMTP with
   example settings or an empty IMAP password.
5. Confirm the provider and hosting account permit inbound TCP 25. From a machine
   outside the VPS network run `nc -vz mx.example.com 25`; then run
   `openssl s_client -starttls smtp -connect mx.example.com:25
   -servername mx.example.com` and verify the certificate.
6. Start `maddy-bootstrap` once (Compose does this automatically). It idempotently
   creates the exact mailbox and updates its IMAP credential. Confirm with
   `docker compose run --rm maddy-bootstrap`.

The `maddy-data` volume contains the durable mailbox. `maddy-backup` writes daily,
14-day snapshots to `maddy-backups`; copy those archives to the normal off-host backup
target. A restart must retain the mailbox before rollout continues.

Configure server-side automatic redirects from Wise and PayPal so the original MIME and
DKIM signature are preserved. Begin with `PAYMENT_EMAIL_MODE=capture`, commit only
sanitized captures as test fixtures, add reviewed exact templates/signing domains, then
use `shadow`. Do not select `active` until shadow results are verified. Activation
requires both the current highest mailbox UID and an RFC3339 time in
`PAYMENT_EMAIL_ACTIVATION_UID`/`PAYMENT_EMAIL_ACTIVATION_TIME`; older messages are
permanently ineligible. If DKIM fails after forwarding, activation remains blocked.

After deployment, query the Fiat Controller with its shared secret:

```bash
curl -H "X-Webhook-Secret: $PUSHPUSH_NOTIFY_SECRET" \
  "https://<fiat-controller>/notification-health?payment_method=Wise"
```

All channels selected on the resolved payment account must be healthy. An account with
no selected channel fails pre-trade.

```bash
# As root on the VPS
useradd -m -s /bin/bash deploy
mkdir -p /home/deploy/.ssh
cp deploy_key.pub /home/deploy/.ssh/authorized_keys
chown -R deploy:deploy /home/deploy/.ssh
chmod 700 /home/deploy/.ssh
chmod 600 /home/deploy/.ssh/authorized_keys

# Install docker + compose plugin (adjust for your distro)
apt-get install -y docker.io docker-compose-plugin
usermod -aG docker deploy

# Clone the repo
git clone https://github.com/<owner>/dexPendulum.git /opt/dexArbTrading

# Authenticate for GHCR pulls (one-time, using a PAT with read:packages)
sudo -u deploy docker login ghcr.io
```

In the GitHub repo settings, add the five secrets listed above. The next push to `main` will deploy — CI will decrypt and upload the `.env` files automatically.
