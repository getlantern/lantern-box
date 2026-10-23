# Tencent CVM images

The `tencentcloud-cvm.lantern-box` Packer source builds Ubuntu 24.04 amd64 in
Hong Kong and uses Packer's native `image_copy_regions` support to publish to
the fixed list in `tencent.pkr.hcl`: Bangkok, Jakarta, Johor, Osaka, Seoul,
Singapore, Tokyo, Frankfurt, Saudi Arabia, Ashburn, Queretaro, Silicon Valley
and São Paulo. Together with Hong Kong, these are the 14 verified image regions.
Mainland regions are excluded because this account cannot create CVMs there.

The builder uses `S5.MEDIUM2` with a 50 GB `CLOUD_PREMIUM` disk. The pinned
Tencent plugin v1.2.0 cannot represent `CLOUD_BSSD`; the resulting image has
been verified on runtime `SA5.MEDIUM2` VMs using `CLOUD_BSSD`.

Shared provisioners install dependencies; cloud-init installs the selected
lantern-box release on first boot. `tencent-cleanup.sh` resets cloud-init,
machine identity and SSH host keys, removes the builder's authorized keys,
and locks the builder user before image capture.

## GitHub build

Add these repository Actions secrets to `getlantern/lantern-box`, using the
Packer identity stored at `secret/lantern_cloud/vps/tencent_packer_api_key`:

- `TENCENTCLOUD_SECRET_ID`
- `TENCENTCLOUD_SECRET_KEY`

Dispatch `.github/workflows/build-images.yaml` with
`builders=tencentcloud-cvm`. Packer builds once in Hong Kong, submits the
fixed destination list, waits for the copies and records their IDs in the
uploaded Packer manifest. Tencent remains opt-in for automatic builds.

## Local build through lantern-cloud

The launcher reads the same Packer identity from staging Vault and passes it
to Packer through environment variables without printing credentials:

```bash
source staging.env
timeout 10m mise r tencent:build-image -- --repo "$HOME/lantern/lantern-box" --version 2026.10.23 --validate
timeout --signal=INT --kill-after=6m 100m mise r tencent:build-image -- --repo "$HOME/lantern/lantern-box" --version 2026.10.23
```

The destination list has one checked-in definition. An explicit
`--copy-regions ap-singapore` overrides it for a limited canary;
`--copy-regions ''` disables copies for a source-only build.

Packer itself can be invoked from `deploy/packer` with
`packer init .` and
`packer build -only=tencentcloud-cvm.lantern-box -var lantern_box_version=2026.10.23 .`.
Its `tencent_region`, `tencent_zone`, `tencent_source_image_id` and
`tencent_instance_type` variables allow deliberate source overrides.

Use a new image version for each build. After an interrupted build, inspect
the Packer output and regional inventory before retrying. Retained custom
images remain subject to account quota; this workflow does not prune them.
