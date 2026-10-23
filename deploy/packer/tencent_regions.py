"""Build regional Tencent seeds and publish images across every available CVM region."""

import argparse
from collections import deque
from concurrent.futures import ThreadPoolExecutor, as_completed
import json
import os
import re
from pathlib import Path
import signal
import subprocess
import sys
import threading
import time
import uuid

from tencent_api import TencentAPI


MAINLAND = {"ap-beijing", "ap-shanghai", "ap-nanjing", "ap-guangzhou", "ap-chengdu", "ap-chongqing", "ap-zhongwei"}
OVERSEAS = {"ap-hongkong", "ap-singapore", "ap-johorbahru", "ap-jakarta", "ap-seoul", "ap-tokyo", "ap-osaka", "ap-bangkok", "me-saudi-arabia", "sa-saopaulo", "na-queretaro", "na-siliconvalley", "na-ashburn", "eu-frankfurt"}
STOP = threading.Event()
FAILED_IMAGE_STATES = {"CREATEFAILED", "IMPORTFAILED", "SYNCFAILED"}


class PackerBuildError(RuntimeError):
    def __init__(self, output):
        codes = re.findall(r"Code=([A-Za-z0-9_.]+)", output)
        self.code = codes[-1] if codes else "BuildFailed"
        super().__init__(output or "Packer failed without diagnostic output")


def realm(region):
    if region in MAINLAND:
        return "mainland"
    if region in OVERSEAS:
        return "overseas"
    return region


def run_packer(command, root):
    process = subprocess.Popen(command, cwd=root, start_new_session=True, stdout=subprocess.PIPE,
                               stderr=subprocess.STDOUT, text=True, bufsize=1)
    output = deque(maxlen=80)
    def drain():
        for line in process.stdout:
            output.append(line.rstrip())
            print(line, end="", flush=True)
    reader = threading.Thread(target=drain, daemon=True)
    reader.start()
    try:
        if process.wait(timeout=2400):
            reader.join(timeout=10)
            raise PackerBuildError("\n".join(output)[-8192:])
    except (KeyboardInterrupt, subprocess.TimeoutExpired):
        os.killpg(process.pid, signal.SIGINT)
        try:
            process.wait(timeout=300)
        except subprocess.TimeoutExpired:
            os.killpg(process.pid, signal.SIGKILL)
            process.wait()
        raise
    finally:
        reader.join(timeout=10)


def submit_copies(api, seed_region, seed_id, targets, version):
    if STOP.is_set():
        raise InterruptedError("regional publication interrupted")
    name = f"lantern-box-{version}-amd64-{uuid.uuid4().hex[:12]}"
    return api.call("SyncImages", {"ImageIds": [seed_id], "DestinationRegions": targets, "ImageSetRequired": True, "ImageName": name}, seed_region)


def wait_image(api, region, image_id, timeout=2400):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if STOP.is_set():
            raise InterruptedError("image readiness wait interrupted")
        images = api.call("DescribeImages", {"ImageIds": [image_id]}, region)["ImageSet"]
        if len(images) == 1:
            state = images[0]["ImageState"]
            if state == "NORMAL":
                return images[0]
            if state in FAILED_IMAGE_STATES:
                raise RuntimeError(f"{region}/{image_id}: image state {state}")
        STOP.wait(10)
    raise TimeoutError(f"{region}/{image_id} did not become NORMAL")

def matching_images(api, region, version):
    name = f"lantern-box-{version}-amd64-"
    return sorted((image for image in api.pages("DescribeImages", {"Filters": [{"Name": "image-type", "Values": ["PRIVATE_IMAGE"]}]}, region, "ImageSet")
                   if (image.get("ImageName") or "").startswith(name) and image.get("Architecture") == "x86_64"
                   and image.get("ImageState") not in FAILED_IMAGE_STATES | {"DELETING"}),
                  key=lambda image: (image.get("CreatedTime") or "", image["ImageId"]), reverse=True)


def build_seed(api, region, args, root):
    existing = matching_images(api, region, args.version)
    if existing and not args.validate:
        return wait_image(api, region, existing[0]["ImageId"])
    images = list(api.pages("DescribeImages", {"Filters": [{"Name": "image-type", "Values": ["PUBLIC_IMAGE"]}, {"Name": "platform", "Values": ["Ubuntu"]}]}, region, "ImageSet"))
    images = sorted((image for image in images if image.get("Architecture") == "x86_64" and image.get("ImageState") == "NORMAL" and "24.04" in (image.get("ImageName") or "")), key=lambda image: (image.get("CreatedTime") or "", image["ImageId"]), reverse=True)
    if not images:
        raise RuntimeError(f"{region}: no Ubuntu 24.04 amd64 public image")
    offers = api.call("DescribeZoneInstanceConfigInfos", {"Filters": [{"Name": "instance-charge-type", "Values": ["POSTPAID_BY_HOUR"]}]}, region)["InstanceTypeQuotaSet"]
    offers = sorted((offer for offer in offers if offer["Status"] == "SELL" and offer["InstanceType"] in {"S5.MEDIUM2", "SA2.MEDIUM2"}), key=lambda offer: (offer["InstanceType"], offer["Zone"]))
    if not offers:
        raise RuntimeError(f"{region}: no S5/SA2.MEDIUM2 builder stock compatible with the pinned Packer plugin")
    offer = offers[0]
    command = [args.packer, "validate" if args.validate else "build", "-only=tencentcloud-cvm.lantern-box",
               "-var", f"lantern_box_version={args.version}", "-var", f"tencent_region={region}",
               "-var", f"tencent_zone={offer['Zone']}", "-var", f"tencent_source_image_id={images[0]['ImageId']}",
               "-var", f"tencent_instance_type={offer['InstanceType']}", "-var", "tencent_copy_regions=[]", "."]
    run_packer(command, root)
    if args.validate:
        return {"validated": True}
    images = matching_images(api, region, args.version)
    if not images:
        raise RuntimeError(f"{region}: Packer succeeded without a discoverable image")
    return wait_image(api, region, images[0]["ImageId"])


def main():
    def interrupted(signum, frame):
        STOP.set()
        raise KeyboardInterrupt
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--version", required=True)
    parser.add_argument("--regions", default="all", help="all or comma-separated CVM regions")
    parser.add_argument("--skip-regions", default="", help="comma-separated regions excluded for this account/run")
    parser.add_argument("--packer", default="packer")
    parser.add_argument("--plan", action="store_true")
    parser.add_argument("--validate", action="store_true")
    parser.add_argument("--report", type=Path, default=Path("tencent-regions-report.json"))
    args = parser.parse_args()
    if len(f"lantern-box-{args.version}-amd64-" + "0" * 12) > 60:
        parser.error("version is too long for Tencent's 60-character image names")
    api = TencentAPI()
    root = Path(__file__).resolve().parent
    discovered = api.call("DescribeRegions", {})["RegionSet"]
    available = sorted(region["Region"] for region in discovered if region["RegionState"] == "AVAILABLE")
    regions = available if args.regions == "all" else sorted(set(args.regions.split(",")))
    if not regions or any(region not in available for region in regions):
        parser.error("regions must be AVAILABLE in CVM DescribeRegions for this account")
    skipped = set(filter(None, args.skip_regions.split(",")))
    if skipped - {region["Region"] for region in discovered}:
        parser.error("skip-regions contains an unknown CVM region")
    excluded = sorted(set(regions) & skipped)
    regions = [region for region in regions if region not in skipped]
    if not regions:
        parser.error("no target regions remain after exclusions")
    groups = {}
    for region in regions:
        groups.setdefault(realm(region), []).append(region)
    plan = {"regions": regions, "groups": groups, "skipped_regions": excluded, "unavailable_regions": {region["Region"]: region["RegionState"] for region in discovered if region["RegionState"] != "AVAILABLE"}, "results": {}}
    def save():
        temporary = args.report.with_suffix(args.report.suffix + ".tmp")
        temporary.write_text(json.dumps(plan, indent=2) + "\n")
        temporary.replace(args.report)
    save()
    if args.plan:
        print(json.dumps(plan, indent=2))
        return 0
    futures = {}
    with ThreadPoolExecutor(max_workers=4) as pool:
        for group, targets in groups.items():
            preferred = "ap-beijing" if group == "mainland" else "ap-hongkong"
            ordered = sorted(targets, key=lambda region: (region != preferred, region))
            seed_region, seed = None, None
            for candidate in ordered:
                try:
                    print(f"Resolving {group} seed in {candidate}", flush=True)
                    seed = build_seed(api, candidate, args, root)
                    seed_region = candidate
                    plan["results"][candidate] = {"status": "validated" if args.validate else "ready", **seed}
                    save()
                    break
                except Exception as error:
                    plan["results"][candidate] = {"status": "failed", "error": str(error)}
                    save()
                    if getattr(error, "code", "") in {"AccountQualificationRestrictions", "InvalidAccount.InsufficientBalance", "AuthFailure.UnauthorizedOperation"}:
                        for target in targets:
                            plan["results"][target] = {"status": "failed", "error": str(error)}
                        save()
                        break
            if seed is None:
                continue
            missing = []
            for target in targets:
                if target == seed_region:
                    continue
                if args.validate:
                    futures[pool.submit(build_seed, api, target, args, root)] = target
                    continue
                try:
                    existing = matching_images(api, target, args.version)
                    if existing:
                        futures[pool.submit(wait_image, api, target, existing[0]["ImageId"])] = target
                    else:
                        missing.append(target)
                except Exception as error:
                    plan["results"][target] = {"status": "failed", "error": str(error)}
                    save()
            if missing:
                try:
                    wait_image(api, seed_region, seed["ImageId"])
                    copied = submit_copies(api, seed_region, seed["ImageId"], missing, args.version)
                    returned = copied.get("ImageSet", [])
                    for target in missing:
                        identifiers = [item["ImageId"] for item in returned if item["Region"] == target]
                        if len(identifiers) != 1:
                            plan["results"][target] = {"status": "failed", "error": "SyncImages returned no unique target ID; recover by name before retry"}
                        else:
                            plan["results"][target] = {"status": "copying", "ImageId": identifiers[0], "source_region": seed_region}
                            futures[pool.submit(wait_image, api, target, identifiers[0])] = target
                    save()
                except Exception as error:
                    for target in missing:
                        plan["results"][target] = {**plan["results"].get(target, {}), "status": "failed", "error": str(error)}
                    save()
        for future in as_completed(futures):
            target = futures[future]
            try:
                result = future.result()
                plan["results"][target] = {**plan["results"].get(target, {}), "status": "validated" if args.validate else "ready", **result}
            except Exception as error:
                plan["results"][target] = {**plan["results"].get(target, {}), "status": "failed", "error": str(error)}
            print(f"{target}: {plan['results'][target]['status']}", flush=True)
            save()
    print(json.dumps({region: result.get("status") for region, result in plan["results"].items()}, indent=2))
    return 0 if len(plan["results"]) == len(regions) and all(result["status"] in {"ready", "validated"} for result in plan["results"].values()) else 1


if __name__ == "__main__":
    sys.exit(main())
