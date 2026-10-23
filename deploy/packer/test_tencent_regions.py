import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest import mock

import tencent_regions as regions


class TencentRegionsTests(unittest.TestCase):
    def test_plan_excludes_requested_regions_without_building(self):
        api = mock.Mock()
        api.call.return_value = {"RegionSet": [{"Region": "ap-hongkong", "RegionState": "AVAILABLE"},
                                                {"Region": "ap-beijing", "RegionState": "AVAILABLE"}]}
        with tempfile.TemporaryDirectory() as directory:
            report = Path(directory, "report.json")
            argv = ["tencent_regions.py", "--version", "test", "--plan", "--skip-regions", "ap-beijing", "--report", str(report)]
            with mock.patch.object(regions.sys, "argv", argv), mock.patch.object(regions, "TencentAPI", return_value=api), mock.patch("builtins.print"):
                self.assertEqual(0, regions.main())
            result = json.loads(report.read_text())
            self.assertEqual(["ap-hongkong"], result["regions"])
            self.assertEqual(["ap-beijing"], result["skipped_regions"])
            api.call.assert_called_once_with("DescribeRegions", {})

    def test_crossborder_and_unknown_regions_are_isolated(self):
        self.assertEqual("mainland", regions.realm("ap-beijing"))
        self.assertEqual("overseas", regions.realm("ap-hongkong"))
        self.assertEqual("ap-shanghai-fsi", regions.realm("ap-shanghai-fsi"))
        self.assertEqual("ap-new-mainland", regions.realm("ap-new-mainland"))

    def test_readiness_uses_the_destination_image_id(self):
        api = mock.Mock()
        api.call.return_value = {"ImageSet": [{"ImageId": "img-target", "ImageState": "NORMAL"}]}
        result = regions.wait_image(api, "ap-singapore", "img-target")
        self.assertEqual("img-target", result["ImageId"])
        api.call.assert_called_once_with("DescribeImages", {"ImageIds": ["img-target"]}, "ap-singapore")

    def test_uncertain_copy_is_not_retried(self):
        api = mock.Mock()
        api.pages.return_value = []
        api.call.side_effect = TimeoutError("outcome unknown")
        with self.assertRaises(TimeoutError):
            regions.submit_copies(api, "ap-hongkong", "img-seed", ["ap-singapore", "ap-tokyo"])
        self.assertEqual(1, api.call.call_count)
        api.call.assert_called_once_with("SyncImages", {"ImageIds": ["img-seed"], "DestinationRegions": ["ap-singapore", "ap-tokyo"], "ImageSetRequired": True}, "ap-hongkong")

    def test_packer_timeout_signals_its_process_group_and_waits_for_cleanup(self):
        process = mock.Mock(pid=123)
        process.stdout = []
        process.wait.side_effect = [subprocess.TimeoutExpired("packer", 2400), 0]
        with mock.patch.object(regions.subprocess, "Popen", return_value=process), mock.patch.object(regions.os, "killpg") as kill:
            with self.assertRaises(subprocess.TimeoutExpired):
                regions.run_packer(["packer"], Path("."))
        kill.assert_called_once_with(123, regions.signal.SIGINT)
        self.assertEqual([mock.call(timeout=2400), mock.call(timeout=300)], process.wait.call_args_list)


if __name__ == "__main__":
    unittest.main()
