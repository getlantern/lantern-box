# Packer Images for lantern-box

Pre-baked VM images with runtime dependencies, systemd drop-ins, and
sidecars (otelcol-contrib, Tailscale) already present. The
`lantern-box` binary itself is **not** baked in — cloud-init apt-installs
it on first boot (Reflog's Option B; see the "Not in the image" section
below). Boot-to-proxy-ready is still fast because the heavy work
(package install, otel config, CA cert, user setup) is done at image
build time; only the small apt-install step runs on first boot.

## What's in the image

- Ubuntu 24.04 LTS
- Runtime deps: ca-certificates, tzdata, nftables, wireguard-tools
- otelcol-contrib + systemd drop-in for host metrics
- systemd drop-ins for lantern-box env (OTel, etc.)
- `/etc/lantern-box/` and `/var/lib/lantern-box/` directories

**Not in the image (Reflog's Option B):** the `lantern-box` binary itself.
Under the central-orchestration design in
`lantern-cloud/docs/design/central-vps-updates.md`, cloud-init
apt-installs the target release tag on first boot — decoupling release
cadence (frequent) from base-image cadence (rare). The packer image is
now version-agnostic; only base-image changes (Ubuntu patches, systemd
drop-in updates, new sidecars) need a rebuild.

**Operators:** before rolling out a new image built from this code,
ensure `bandit_vps_default_release_tag` (or a per-track override) is set
in the lantern-cloud settings. Otherwise new VMs boot without lantern-box
installed and the provision worker's `systemctl enable --now lantern-box`
call will fail.

## Prerequisites

- [Packer](https://developer.hashicorp.com/packer/install) installed
- API tokens for target cloud providers
- `FURY_TOKEN` for the Gemfury .deb repo

## Build locally

```bash
cd deploy/packer
packer init .

# Build for a single provider
packer build \
  -var "lantern_box_version=0.5.0" \
  -only="linode.lantern-box" \
  .

# Build for all providers
packer build \
  -var "lantern_box_version=0.5.0" \
  .
```

### Datacap (optional, closed-source)

In CI, the `datacap` binary is built from `getlantern/lantern-cloud` and baked into the image. For local builds, empty placeholders are created automatically so the build succeeds without it. To include datacap locally, place the pre-built binaries at `/tmp/datacap-amd64` and `/tmp/datacap-arm64` before running `packer build`.

## Environment variables

| Variable                      | Description                                                                |
| ----------------------------- | -------------------------------------------------------------------------- |
| `LINODE_TOKEN`                | Linode/Akamai API token                                                    |
| `FURY_TOKEN`                  | Gemfury token for .deb repo                                                |
| `OCI_TENANCY_OCID`            | OCI tenancy OCID                                                           |
| `OCI_USER_OCID`               | OCI user OCID                                                              |
| `OCI_FINGERPRINT`             | OCI API key fingerprint                                                    |
| `OCI_KEY_CONTENT`             | OCI API private key (PEM)                                                  |
| `OCI_COMPARTMENT_OCID`        | OCI compartment for the image                                              |
| `OCI_SUBNET_OCID`             | Legacy fallback subnet (used by IAD if `OCI_SUBNET_OCID_IAD` is empty)     |
| `OCI_AVAILABILITY_DOMAIN`     | Legacy fallback AD (used by IAD if `OCI_AVAILABILITY_DOMAIN_IAD` is empty) |
| `OCI_SUBNET_OCID_IAD`         | OCI subnet — us-ashburn-1                                                  |
| `OCI_AVAILABILITY_DOMAIN_IAD` | OCI AD — us-ashburn-1                                                      |
| `OCI_SUBNET_OCID_FRA`         | OCI subnet — eu-frankfurt-1                                                |
| `OCI_AVAILABILITY_DOMAIN_FRA` | OCI AD — eu-frankfurt-1                                                    |
| `OCI_SUBNET_OCID_NRT`         | OCI subnet — ap-tokyo-1                                                    |
| `OCI_AVAILABILITY_DOMAIN_NRT` | OCI AD — ap-tokyo-1                                                        |
| `OCI_SUBNET_OCID_SIN`         | OCI subnet — ap-singapore-1                                                |
| `OCI_AVAILABILITY_DOMAIN_SIN` | OCI AD — ap-singapore-1                                                    |
| `OCI_SUBNET_OCID_PHX`         | OCI subnet — us-phoenix-1                                                  |
| `OCI_AVAILABILITY_DOMAIN_PHX` | OCI AD — us-phoenix-1                                                      |
| `OCI_SUBNET_OCID_AMS`         | OCI subnet — eu-amsterdam-1                                                |
| `OCI_AVAILABILITY_DOMAIN_AMS` | OCI AD — eu-amsterdam-1                                                    |
| `OCI_SUBNET_OCID_BOM`         | OCI subnet — ap-mumbai-1                                                   |
| `OCI_AVAILABILITY_DOMAIN_BOM` | OCI AD — ap-mumbai-1                                                       |
| `OCI_SUBNET_OCID_GRU`         | OCI subnet — sa-saopaulo-1                                                 |
| `OCI_AVAILABILITY_DOMAIN_GRU` | OCI AD — sa-saopaulo-1                                                     |
| `GCORE_API_KEY`               | Gcore Cloud API token (sent as `Authorization: APIKey ...`)                |
| `GCORE_PROJECT_ID`            | Gcore project ID (numeric); scopes every API call                          |

## Deploy a VPS from the image

1. Create a VPS using the snapshot/image from the build
2. Pass a cloud-init user-data file that writes the config and starts the service

See `cloud-init.yaml.example` for the template.

### Linode example

```bash
linode-cli linodes create \
  --image private/<image-id> \
  --region us-west \
  --type g6-nanode-1 \
  --metadata.user_data "$(base64 -w0 cloud-init.yaml)" \
  --label my-proxy
```

## CI

The `build-images.yaml` workflow runs automatically when a GitHub release is published. It can also be triggered manually via workflow_dispatch.

## Adding a new cloud provider

1. Add a `required_plugins` block and `source` block in `lantern-box.pkr.hcl`
2. Add the provider's API token as a variable
3. Add the new source to the `build.sources` list
4. Add the secret to the CI workflow

## Gcore

gcore has **no Packer plugin** and **no cross-region image-copy API**, so unlike
the other builders (whose plugins snapshot the built instance straight into the
provider's image store) gcore uses a **build → host → import** pipeline:

1. The `qemu.lantern-box-gcore` Packer source builds one qcow2 from the stock
   Ubuntu 24.04 cloud image, reusing the **same** provisioners as every other
   builder (so image contents match), then generalizes the disk (cloud-init
   clean, zero machine-id, drop SSH host keys, lock the build password) so the
   raw disk boots fresh on import.
2. CI stages the qcow2 in **gcore's own S3-compatible object storage**, in a
   throwaway, **randomly-named** bucket made briefly public-read for the import
   and then torn down — no external cloud, no stored bucket/key secret. Public is
   required: gcore's importer fetches an **unauthenticated** URL (HEAD then GET),
   the API has no field for source credentials, and a presigned SigV4 URL is bound
   to one method (its HEAD preflight 403s). `deploy/packer/gcore-storage.sh` drives
   the gcore control plane — find-or-create a dedicated storage instance
   (`lantern-box-images`, location `luxembourg-2`), mint an **ephemeral** access key,
   sweep leftover stage buckets with it, create a fresh `lantern-box-stage-<random>`
   bucket — and the AWS CLI, as a generic S3 client pointed at gcore's endpoint,
   uploads the qcow2 and applies an anonymous `s3:GetObject` policy. gcore is
   handed the plain `https://<region>.cloud.gcore.lu/<bucket>/<key>` URL. `always()`
   cleanup steps then revoke the policy, delete the object, and delete the bucket
   and the access key — in that order.

   The tradeoff: world-readable for the length of the imports only, with an
   unguessable bucket name holding just that one qcow2, the URL masked out of CI
   logs, and teardown running even on failure. Teardown revokes the bucket policy
   **first**, before either delete, because that is the step that actually ends the
   exposure and it works even on a bucket the deletes would refuse — so a failed or
   cancelled run closes the public window regardless. Only a lost runner, where no
   teardown runs at all, leaves a public bucket behind; the next run's sweep revokes
   its policy, empties it, and deletes it.

   Every one of those checks fails **loudly on an inconclusive result** rather than
   reporting the reassuring one. If the revoke fails and the policy cannot be read
   back either (revoked credentials, an endpoint blip), the step does not conclude
   "no policy was set" — it fails the job and names the storage instance to check.
   Likewise, if the bucket delete is refused and the bucket listing cannot be read,
   cleanup does not report "already gone". Bucket names are masked in CI logs, so
   these messages point at the `lantern-box-stage-*` prefix and the instance ID
   instead of a name that would render as `***`.

   Two backstops sit under that. The stage bucket is created with a **1-day object
   expiry** lifecycle rule (gcore implements the legacy `put-bucket-lifecycle`), so
   even a run that dies before any teardown has the qcow2 removed by gcore itself —
   which also un-sticks the bucket, since the control-plane delete refuses a non-empty
   one. Gcore's expiry pass runs around midnight UTC and lands a day later than `Days`
   implies, so the worst case is ~48h: a floor under the exposure, not a substitute for
   the revoke, which closes it in seconds. And because the multi-GB upload is
   multipart, emptying a bucket **aborts incomplete multipart uploads first** — `s3 rm`
   does not touch the parts left by a killed upload, and the expiry rule does not cover
   them either (that needs `AbortIncompleteMultipartUpload`, which gcore does not
   document), so without the abort such a bucket could never be emptied or deleted.
   Both the teardown and the sweep go through `gcore-storage.sh empty-bucket`, so the
   two paths empty a bucket identically.

   Runs are serialized per builder (`concurrency: build-images-<builder>`). The
   storage instance is shared, so provisioning prunes _all_ its access keys and
   sweeps _all_ its stage buckets — two overlapping gcore runs would revoke each
   other's credentials mid-upload. `cancel-in-progress` stays `false`: cancelling a
   gcore build in flight is the one thing that can strand a public bucket.

3. `deploy/packer/gcore-publish.sh` imports that URL into each target region via
   `POST cloud/v1/downloadimage/{project}/{region}` (name `lantern-box-<version>`,
   `architecture=x86_64`, `os_type=linux`), polls each task to `FINISHED`, and checks
   the created image's `visibility`.

**Image visibility** is read-only in gcore's API: neither
`POST cloud/v1/downloadimage/...` nor `PATCH cloud/v1/images/...` accepts the
field, and `private`/`visibility` exist only as _list filters_. It is not a
boolean — an image is exactly one of:

- `private` — this project only.
- `shared` — this project plus any project added as a member. Gcore exposes no
  image-member API, so in practice this is also project-only. **Not** public.
- `public` — every gcore customer; gcore's global catalog.

Since the field cannot be set, step 3 acts on it instead. `private` is logged.
`public` is an exposure and the only remediation gcore leaves is removal, so the
image is **deleted** and the publish fails either way — the region ends up with no
usable private image, and if the delete itself fails the log says so explicitly and
names it as needing a human. `shared` only warns: it is not public, and it is still
usable, so deleting it would cost an image for no security gain — and prune's
two-pass listing (below) still collects it, so it does not escape retention either.

The check **fails closed**. Both lookups behind it — the image ID from the import
task, then that image's `visibility` — retry `VISIBILITY_ATTEMPTS` times (default 3)
and then fail the publish for that region. An unreadable answer means nobody knows
whether the image reached gcore's global catalog, and this is the only check that
would catch it, so treating "could not tell" as a pass would turn the exposure into
a green build. The bounded retry is what keeps one API blip from failing an
otherwise good build; exhausting it names the image so a human can check it.

**Regions** are numeric gcore region IDs. The default list — every region
lantern-cloud provisions in — lives in exactly one place, the `prepare` job's
"Resolve gcore regions and build timeout" step, and both push-to-main and
default dispatches use it. lantern-cloud's provider resolves the boot image per
region and fails where one is missing, so publishing to a subset is what caused
the `no gcore image matching prefix "lantern-box-" in region 180` failure this
builder fixes. The `gcore_regions` dispatch input overrides the list for a scoped
test (e.g. a single region to verify a change) and is intentionally left blank
rather than prefilled, so there is no second copy to drift. The prune job keeps
the 3 newest `lantern-box-` images per region, over the same list.

**The build-time SSH password is not a secret.** Packer needs a password to SSH
into the stock cloud image; the generalize step locks the account, but its hash
still ships in the qcow2 — and that qcow2 is briefly world-readable while gcore
imports it. So the build job generates a fresh `openssl rand -hex 24` per run and
masks it. A stored secret's hash would remain valid for every later build if that
window were ever caught; a per-run value is worthless once the job ends. Keep any
replacement hex or alphanumeric: it is interpolated into the cloud-init YAML and
into `shutdown_command`'s single-quoted shell string.

**Imports run concurrently.** There is one staged object and one URL, and each region's
importer fetches it independently, so the transfers were never serial — only the waiting
was. `gcore-publish.sh` starts every region's import first, then polls all outstanding
tasks together, one sleep per round rather than per region. Wall-clock is the slowest
single region instead of the sum, which also shortens the window the qcow2 spends
publicly readable. A region whose import is rejected is counted as a failure without
holding up the others.

The build job's timeout is budgeted in the `prepare` job (55min fixed + a 35min shared
poll window + 3min per region, capped at 350) rather than in `timeout-minutes`, which
cannot do arithmetic. Adding regions therefore needs no timeout edit and costs little
wall-clock. The per-region term is not the poll window — it covers the sequential import
POSTs and visibility checks plus the fact that every region pulls the same object from one
bucket at once, so shared egress makes each import slower than a solo one.

With a long region list the binding constraint is **not** this timeout but `POLL_ATTEMPTS`
(120 x 15s = 30min per task): if shared bucket egress pushes a single import past that,
its poll gives up however long the job is allowed to run. That is the knob to raise
first, and getting killed mid-import is worth avoiding since it leaves orphaned gcore
tasks.

Prune lists each region **twice** — `?private=true` plus unfiltered — and unions the
results by image ID, because a `shared` or `public` image never appears in the
private listing. Without the second pass such an image is invisible to prune and
accumulates forever. Both passes are kept rather than only the unfiltered one, so
pruning does not depend on whether gcore's default listing happens to include private
images. The `lantern-box-` name prefix is what keeps gcore's own public catalog out
of the union, and a `403` on delete is treated like `404` — another project's public
image can match the prefix, and that is not ours to delete or to fail on.

**Local build:** the QEMU build requires **Linux + KVM** (`/dev/kvm`), so it runs
in CI, not on macOS. To validate the config anywhere:
`cd deploy/packer && packer init . && packer validate -only=qemu.lantern-box-gcore -var lantern_box_version=0.0.0 .`

**Operator prerequisites (one-time):**

- Repo secrets: `GCORE_API_KEY` (must have **object-storage** permission, not just
  cloud/compute) and `GCORE_PROJECT_ID`. There is no build-password secret: the
  build job mints a random one per run (see below).

That's it — the storage instance, the per-run stage bucket, and the (ephemeral)
access keys are created and torn down by the workflow itself
(`gcore-storage.sh`), so there's no bucket to pre-create and no cloud IAM to
configure.
