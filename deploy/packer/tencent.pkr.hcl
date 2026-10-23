variable "tencent_region" {
  type    = string
  default = "ap-hongkong"
}

variable "tencent_zone" {
  type    = string
  default = "ap-hongkong-2"
}

variable "tencent_source_image_id" {
  type    = string
  default = "img-mmytdhbn"
}

variable "tencent_instance_type" {
  type    = string
  default = "S5.MEDIUM2"
}

variable "tencent_copy_regions" {
  type = list(string)
  default = [
    "ap-bangkok",
    "ap-jakarta",
    "ap-johorbahru",
    "ap-osaka",
    "ap-seoul",
    "ap-singapore",
    "ap-tokyo",
    "eu-frankfurt",
    "me-saudi-arabia",
    "na-ashburn",
    "na-queretaro",
    "na-siliconvalley",
    "sa-saopaulo",
  ]
}

source "tencentcloud-cvm" "lantern-box" {
  # The archived plugin's region allowlist predates four verified copy destinations.
  skip_region_validation = true
  region                 = var.tencent_region
  zone                   = var.tencent_zone
  instance_type          = var.tencent_instance_type
  source_image_id        = var.tencent_source_image_id
  instance_charge_type   = "POSTPAID_BY_HOUR"
  instance_name          = "lantern-box-packer"
  image_name             = "lantern-box-${var.lantern_box_version}-amd64-{{timestamp}}"
  image_copy_regions     = var.tencent_copy_regions
  # The v1.2.0 builder cannot represent CLOUD_BSSD; S5 supports CLOUD_PREMIUM.
  disk_type                   = "CLOUD_PREMIUM"
  disk_size                   = 50
  associate_public_ip_address = true
  internet_charge_type        = "TRAFFIC_POSTPAID_BY_HOUR"
  internet_max_bandwidth_out  = 10
  ssh_username                = "ubuntu"
  ssh_timeout                 = "15m"
  cvm_endpoint                = "https://cvm.tencentcloudapi.com"
  vpc_endpoint                = "https://vpc.tencentcloudapi.com"
  image_tags = {
    managed_by = "lantern-box-packer"
  }
  run_tags = {
    managed_by = "lantern-box-packer"
    # Build VMs are outside the prod/staging fleet orphan reapers.
    environment = "image-build"
  }
}
