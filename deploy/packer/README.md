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

| Variable | Description |
|---|---|
| `LINODE_TOKEN` | Linode/Akamai API token |
| `FURY_TOKEN` | Gemfury token for .deb repo |
| `OCI_TENANCY_OCID` | OCI tenancy OCID |
| `OCI_USER_OCID` | OCI user OCID |
| `OCI_FINGERPRINT` | OCI API key fingerprint |
| `OCI_KEY_CONTENT` | OCI API private key (PEM) |
| `OCI_COMPARTMENT_OCID` | OCI compartment for the image |
| `OCI_SUBNET_OCID` | Legacy fallback subnet (used by IAD if `OCI_SUBNET_OCID_IAD` is empty) |
| `OCI_AVAILABILITY_DOMAIN` | Legacy fallback AD (used by IAD if `OCI_AVAILABILITY_DOMAIN_IAD` is empty) |
| `OCI_SUBNET_OCID_IAD` | OCI subnet — us-ashburn-1 |
| `OCI_AVAILABILITY_DOMAIN_IAD` | OCI AD — us-ashburn-1 |
| `OCI_SUBNET_OCID_FRA` | OCI subnet — eu-frankfurt-1 |
| `OCI_AVAILABILITY_DOMAIN_FRA` | OCI AD — eu-frankfurt-1 |
| `OCI_SUBNET_OCID_NRT` | OCI subnet — ap-tokyo-1 |
| `OCI_AVAILABILITY_DOMAIN_NRT` | OCI AD — ap-tokyo-1 |
| `OCI_SUBNET_OCID_SIN` | OCI subnet — ap-singapore-1 |
| `OCI_AVAILABILITY_DOMAIN_SIN` | OCI AD — ap-singapore-1 |
| `OCI_SUBNET_OCID_PHX` | OCI subnet — us-phoenix-1 |
| `OCI_AVAILABILITY_DOMAIN_PHX` | OCI AD — us-phoenix-1 |
| `OCI_SUBNET_OCID_AMS` | OCI subnet — eu-amsterdam-1 |
| `OCI_AVAILABILITY_DOMAIN_AMS` | OCI AD — eu-amsterdam-1 |
| `OCI_SUBNET_OCID_BOM` | OCI subnet — ap-mumbai-1 |
| `OCI_AVAILABILITY_DOMAIN_BOM` | OCI AD — ap-mumbai-1 |
| `OCI_SUBNET_OCID_GRU` | OCI subnet — sa-saopaulo-1 |
| `OCI_AVAILABILITY_DOMAIN_GRU` | OCI AD — sa-saopaulo-1 |
| `GCORE_API_KEY` | Gcore Cloud API token (sent as `Authorization: APIKey ...`) |
| `GCORE_PROJECT_ID` | Gcore project ID (numeric); scopes every API call |
| `QEMU_SSH_PASSWORD` | Build-time password for the ubuntu user in the QEMU (gcore) build |
| `GCORE_IMAGE_GCS_BUCKET` | Private GCS bucket the built qcow2 is uploaded to for import |

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

## gcore

gcore has **no Packer plugin** and **no cross-region image-copy API**, so unlike
the other builders (whose plugins snapshot the built instance straight into the
provider's image store) gcore uses a **build → host → import** pipeline:

1. The `qemu.lantern-box-gcore` Packer source builds one qcow2 from the stock
   Ubuntu 24.04 cloud image, reusing the **same** provisioners as every other
   builder (so image contents match), then generalizes the disk (cloud-init
   clean, zero machine-id, drop SSH host keys, lock the build password) so the
   raw disk boots fresh on import.
2. CI uploads the qcow2 to a private GCS bucket and mints a 24h V4 signed URL
   (reusing the repo's existing Workload Identity Federation to the
   `lantern-cloud` GCP project — no static key).
3. `deploy/packer/gcore-publish.sh` imports that URL into each target region via
   `POST cloud/v1/downloadimage/{project}/{region}` (name `lantern-box-<version>`,
   `architecture=x86_64`, `os_type=linux`) and polls each task to `FINISHED`.

lantern-cloud's gcore provider then resolves the boot image by listing private
images per region, keeping names prefixed `lantern-box-`, and picking the newest
(x86_64, no fallback).

**Regions** are numeric gcore region IDs. The workflow's `gcore_regions` input
(default `180` = Frankfurt-2) controls where the image is published; the prune
job keeps the 3 newest `lantern-box-` images per region.

**Local build:** the QEMU build requires **Linux + KVM** (`/dev/kvm`), so it runs
in CI, not on macOS. To validate the config anywhere:
`cd deploy/packer && packer init . && packer validate -only=qemu.lantern-box-gcore -var lantern_box_version=0.0.0 .`

**Operator prerequisites (one-time):**

- Repo secrets: `GCORE_API_KEY`, `GCORE_PROJECT_ID`, `QEMU_SSH_PASSWORD`,
  `GCORE_IMAGE_GCS_BUCKET`.
- GCP (lantern-cloud project): create the private GCS bucket (add a short
  object-expiry lifecycle rule — the qcow2 in GCS is only needed transiently
  during import); grant `ghactions@lantern-cloud.iam.gserviceaccount.com`
  `roles/storage.objectAdmin` on the bucket and `roles/iam.serviceAccountTokenCreator`
  **on itself** so `gcloud storage sign-url` can sign via IAM SignBlob under WIF.

## Adding a new cloud provider

1. Add a `required_plugins` block and `source` block in `lantern-box.pkr.hcl`
2. Add the provider's API token as a variable
3. Add the new source to the `build.sources` list
4. Add the secret to the CI workflow
