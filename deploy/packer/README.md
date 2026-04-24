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
