# Packer Images for lantern-box

Pre-baked VM images with lantern-box installed. Boot-to-proxy-ready in ~35-60 seconds (vs 2-4 minutes with a stock image).

## What's in the image

- Ubuntu 24.04 LTS
- Runtime deps: ca-certificates, tzdata, nftables, wireguard-tools
- lantern-box binary (from Gemfury .deb)
- systemd service (installed but not enabled — cloud-init starts it)

## Prerequisites

- [Packer](https://developer.hashicorp.com/packer/install) installed
- API tokens for target cloud providers
- `FURY_TOKEN` for the Gemfury .deb repo

## Build locally

```bash
cd deploy/packer
packer init lantern-box.pkr.hcl

# Build for a single provider
packer build \
  -var "lantern_box_version=0.5.0" \
  -only="digitalocean.lantern-box" \
  lantern-box.pkr.hcl

# Build for all providers
packer build \
  -var "lantern_box_version=0.5.0" \
  lantern-box.pkr.hcl
```

## Environment variables

| Variable | Description |
|---|---|
| `DIGITALOCEAN_API_TOKEN` | DigitalOcean API token |
| `LINODE_TOKEN` | Linode/Akamai API token |
| `FURY_TOKEN` | Gemfury token for .deb repo |
| `OCI_TENANCY_OCID` | OCI tenancy OCID |
| `OCI_USER_OCID` | OCI user OCID |
| `OCI_FINGERPRINT` | OCI API key fingerprint |
| `OCI_KEY_CONTENT` | OCI API private key (PEM) |
| `OCI_COMPARTMENT_OCID` | OCI compartment for the image |
| `OCI_SUBNET_OCID` | OCI subnet for the build instance |
| `OCI_AVAILABILITY_DOMAIN` | OCI availability domain |

## Deploy a VPS from the image

1. Create a VPS using the snapshot/image from the build
2. Pass a cloud-init user-data file that writes the config and starts the service

See `cloud-init.yaml.example` for the template.

### DigitalOcean example

```bash
doctl compute droplet create my-proxy \
  --image <snapshot-id> \
  --region sfo3 \
  --size s-1vcpu-1gb \
  --user-data-file cloud-init.yaml \
  --wait
```

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
