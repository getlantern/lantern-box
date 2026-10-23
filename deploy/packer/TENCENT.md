# Tencent CVM images

The `tencentcloud-cvm.lantern-box` source uses the shared image provisioners and
the pinned HashiCorp Tencent plugin v1.2.0. It builds an Ubuntu 24.04 amd64 image
in a regional seed selected by `tencent_regions.py`. The publisher discovers every
`AVAILABLE` CVM region and copies the image within its mainland or overseas group.
Financial, specialized and unknown regions get independent seeds. Mainland and
overseas images are built separately because cross-border copies require account
enablement: https://cloud.tencent.com/document/product/213/17276.
The source is opt-in in the
GitHub build workflow; it is not part of push-triggered default builds.

The builder uses `S5.MEDIUM2` with a 50 GB `CLOUD_PREMIUM` disk. The archived
plugin's disk validation does not support `CLOUD_BSSD`, which newer `SA5`
instances require. The runtime may launch the resulting portable image with
`CLOUD_BSSD`; validate that path in a canary before rollout. Tencent's disk
constraints are documented at https://cloud.tencent.com/document/product/362/5145.

The shared provisioner installs dependencies; cloud-init installs the selected
lantern-box release at first boot. The separate `tencent-cleanup.sh` clears cached
cloud-init state, machine identity, host keys and temporary authorized keys.

## Local build through lantern-cloud

The separate builder CAM key lives in staging Vault at
`secret/lantern_cloud/vps/tencent_packer_api_key`, fields `secret_id` and
`secret_key`. Optional STS credentials also require `token` and RFC3339
`expires_at`, valid through the selected execution budget and cleanup: at least
four hours and five minutes for all-region publication. Keep these distinct from runtime keys.

From the lantern-cloud checkout:

```bash
source staging.env
timeout 2m mise r tencent:build-image -- --repo "$HOME/lantern/lantern-box" --version 2026.10.22 --plan
timeout 10m mise r tencent:build-image -- --repo "$HOME/lantern/lantern-box" --version 2026.10.22 --regions ap-hongkong,ap-singapore --validate
timeout --signal=INT --kill-after=6m 4h mise r tencent:build-image -- --repo "$HOME/lantern/lantern-box" --version 2026.10.22 --report /tmp/tencent-image-regions.json
```

The task initializes plugins, fetches the builder secret without printing it,
and passes credentials in the child process's environment. It gives Packer a
grace period to clean up its temporary resources if interrupted. Always inspect
VM/network/image inventory after a failure or forced termination.

`--regions` defaults to `all`; an explicit comma-separated subset limits the run.
Unavailable regions are recorded in the report. `--skip-regions` excludes an
explicit comma-separated list for accounts lacking regional purchase qualification.
The publisher submits destination copies in one batch per source because Tencent
marks that source `SYNCING` while copying. It polls up to four destination images
concurrently, waits for each to become `NORMAL`, and
returns a failure if any available target is missing. Rerunning the same version
adopts existing regional images rather than submitting another build or copy.
Region availability does not guarantee account entitlement, source-image access
or compatible builder stock; each failure remains visible in the report.

For a single direct Packer build, pass `--region`, `--zone` and optionally
`--copy-regions`; direct HCL defaults do not copy images. The regional publisher
discovers Ubuntu 24.04 amd64 public images and S5/SA2 builder stock per seed region.

Override `tencent_region`, `tencent_zone`, `tencent_source_image_id`,
`tencent_instance_type` and `tencent_copy_regions` when invoking Packer directly.
All plugins and the full template are loaded with `packer init .`; select only
`-only=tencentcloud-cvm.lantern-box` for Tencent builds.

## GitHub workflow

Set repository or environment secrets `TENCENTCLOUD_SECRET_ID` and
`TENCENTCLOUD_SECRET_KEY` to the builder identity, plus
`TENCENTCLOUD_SECURITY_TOKEN` only for temporary credentials. Then dispatch
`.github/workflows/build-images.yaml` with `builders=tencentcloud-cvm`.
The `tencent_regions` input defaults to `all`; the workflow uploads the regional
result report even after a failure and reserves a 300-minute execution window.
Vault secrets are not automatically available to GitHub Actions.

Before adding Tencent to automatic push builds, validate a complete build/copy,
runtime canary and teardown in the target regions, and implement image retention so
repeated builds cannot exhaust custom-image quota. The current workflow does not
prune Tencent images.
