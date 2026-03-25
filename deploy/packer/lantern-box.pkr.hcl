packer {
  required_plugins {
    digitalocean = {
      version = ">= 1.4.0"
      source  = "github.com/digitalocean/digitalocean"
    }
    linode = {
      version = ">= 1.1.0"
      source  = "github.com/linode/linode"
    }
    oracle = {
      version = ">= 1.0.5"
      source  = "github.com/hashicorp/oracle"
    }
    alicloud = {
      version = ">= 1.1.2"
      source  = "github.com/hashicorp/alicloud"
    }
  }
}

# ---------- Variables ----------

variable "lantern_box_version" {
  type        = string
  description = "Version tag to install (e.g. 1.2.3). Downloaded from GitHub release."
}

variable "do_api_token" {
  type      = string
  sensitive = true
  default   = env("DIGITALOCEAN_API_TOKEN")
}

variable "linode_token" {
  type      = string
  sensitive = true
  default   = env("LINODE_TOKEN")
}


variable "do_region" {
  type    = string
  default = "sfo3"
}

variable "linode_region" {
  type    = string
  default = "us-west"
}

# Alibaba Cloud (Alicloud) variables
variable "alicloud_access_key" {
  type      = string
  sensitive = true
  default   = env("ALICLOUD_ACCESS_KEY")
}

variable "alicloud_secret_key" {
  type      = string
  sensitive = true
  default   = env("ALICLOUD_SECRET_KEY")
}

variable "alicloud_ssh_password" {
  type      = string
  sensitive = true
  default   = env("ALICLOUD_SSH_PASSWORD")
}

# OCI shared variables (same across all regions)
variable "oci_tenancy_ocid" {
  type      = string
  sensitive = true
  default   = env("OCI_TENANCY_OCID")
}

variable "oci_user_ocid" {
  type      = string
  sensitive = true
  default   = env("OCI_USER_OCID")
}

variable "oci_fingerprint" {
  type      = string
  sensitive = true
  default   = env("OCI_FINGERPRINT")
}

variable "oci_key_file" {
  type        = string
  default     = env("OCI_KEY_FILE")
  description = "Path to OCI API private key PEM file."
}

variable "oci_compartment_ocid" {
  type    = string
  default = env("OCI_COMPARTMENT_OCID")
}

# OCI per-region variables: us-ashburn-1 (IAD)
# Falls back to legacy OCI_SUBNET_OCID / OCI_AVAILABILITY_DOMAIN for backward compatibility.
variable "oci_subnet_ocid_iad" {
  type    = string
  default = env("OCI_SUBNET_OCID_IAD") != "" ? env("OCI_SUBNET_OCID_IAD") : env("OCI_SUBNET_OCID")
}

variable "oci_availability_domain_iad" {
  type    = string
  default = env("OCI_AVAILABILITY_DOMAIN_IAD") != "" ? env("OCI_AVAILABILITY_DOMAIN_IAD") : env("OCI_AVAILABILITY_DOMAIN")
}

# OCI per-region variables: eu-frankfurt-1 (FRA)
variable "oci_subnet_ocid_fra" {
  type    = string
  default = env("OCI_SUBNET_OCID_FRA")
}

variable "oci_availability_domain_fra" {
  type    = string
  default = env("OCI_AVAILABILITY_DOMAIN_FRA")
}

# OCI per-region variables: ap-tokyo-1 (NRT)
variable "oci_subnet_ocid_nrt" {
  type    = string
  default = env("OCI_SUBNET_OCID_NRT")
}

variable "oci_availability_domain_nrt" {
  type    = string
  default = env("OCI_AVAILABILITY_DOMAIN_NRT")
}

# OCI per-region variables: ap-singapore-1 (SIN)
variable "oci_subnet_ocid_sin" {
  type    = string
  default = env("OCI_SUBNET_OCID_SIN")
}

variable "oci_availability_domain_sin" {
  type    = string
  default = env("OCI_AVAILABILITY_DOMAIN_SIN")
}

# ---------- Sources ----------

source "digitalocean" "lantern-box" {
  api_token    = var.do_api_token
  image        = "ubuntu-24-04-x64"
  region       = var.do_region
  size         = "s-1vcpu-1gb"
  ssh_username = "root"
  snapshot_name = "lantern-box-${var.lantern_box_version}-{{timestamp}}"
  snapshot_regions = [
    "sfo3", "nyc3", "ams3", "sgp1", "lon1", "fra1", "blr1", "syd1",
  ]
  tags = ["lantern-box", "packer"]
  state_timeout = "10m"
}

# OCI sources — one per region. All share auth + compartment but use
# region-specific subnet, AD, and produce region-local images.
# See: https://developer.hashicorp.com/packer/integrations/hashicorp/oracle/latest/components/builder/oci

source "oracle-oci" "lantern-box-iad" {
  tenancy_ocid        = var.oci_tenancy_ocid
  user_ocid           = var.oci_user_ocid
  fingerprint         = var.oci_fingerprint
  key_file            = var.oci_key_file
  compartment_ocid    = var.oci_compartment_ocid
  availability_domain = var.oci_availability_domain_iad
  region              = "us-ashburn-1"

  base_image_filter {
    display_name_search = "^Canonical-Ubuntu-24.04-Minimal-aarch64-"
    operating_system    = "Canonical Ubuntu"
  }
  shape = "VM.Standard.A1.Flex"
  shape_config {
    ocpus         = 1
    memory_in_gbs = 1
  }
  subnet_ocid  = var.oci_subnet_ocid_iad
  ssh_username = "ubuntu"

  image_name = "lantern-box-${var.lantern_box_version}-arm64-iad"
}

source "oracle-oci" "lantern-box-fra" {
  tenancy_ocid        = var.oci_tenancy_ocid
  user_ocid           = var.oci_user_ocid
  fingerprint         = var.oci_fingerprint
  key_file            = var.oci_key_file
  compartment_ocid    = var.oci_compartment_ocid
  availability_domain = var.oci_availability_domain_fra
  region              = "eu-frankfurt-1"

  base_image_filter {
    display_name_search = "^Canonical-Ubuntu-24.04-Minimal-aarch64-"
    operating_system    = "Canonical Ubuntu"
  }
  shape = "VM.Standard.A1.Flex"
  shape_config {
    ocpus         = 1
    memory_in_gbs = 1
  }
  subnet_ocid  = var.oci_subnet_ocid_fra
  ssh_username = "ubuntu"

  image_name = "lantern-box-${var.lantern_box_version}-arm64-fra"
}

source "oracle-oci" "lantern-box-nrt" {
  tenancy_ocid        = var.oci_tenancy_ocid
  user_ocid           = var.oci_user_ocid
  fingerprint         = var.oci_fingerprint
  key_file            = var.oci_key_file
  compartment_ocid    = var.oci_compartment_ocid
  availability_domain = var.oci_availability_domain_nrt
  region              = "ap-tokyo-1"

  base_image_filter {
    display_name_search = "^Canonical-Ubuntu-24.04-Minimal-aarch64-"
    operating_system    = "Canonical Ubuntu"
  }
  shape = "VM.Standard.A1.Flex"
  shape_config {
    ocpus         = 1
    memory_in_gbs = 1
  }
  subnet_ocid  = var.oci_subnet_ocid_nrt
  ssh_username = "ubuntu"

  image_name = "lantern-box-${var.lantern_box_version}-arm64-nrt"
}

source "oracle-oci" "lantern-box-sin" {
  tenancy_ocid        = var.oci_tenancy_ocid
  user_ocid           = var.oci_user_ocid
  fingerprint         = var.oci_fingerprint
  key_file            = var.oci_key_file
  compartment_ocid    = var.oci_compartment_ocid
  availability_domain = var.oci_availability_domain_sin
  region              = "ap-singapore-1"

  base_image_filter {
    display_name_search = "^Canonical-Ubuntu-24.04-Minimal-aarch64-"
    operating_system    = "Canonical Ubuntu"
  }
  shape = "VM.Standard.A1.Flex"
  shape_config {
    ocpus         = 1
    memory_in_gbs = 1
  }
  subnet_ocid  = var.oci_subnet_ocid_sin
  ssh_username = "ubuntu"

  image_name = "lantern-box-${var.lantern_box_version}-arm64-sin"
}

source "linode" "lantern-box" {
  linode_token  = var.linode_token
  image         = "linode/ubuntu24.04"
  region        = var.linode_region
  instance_type = "g6-nanode-1"
  ssh_username  = "root"
  image_label   = "lantern-box-${var.lantern_box_version}"
  image_description = "lantern-box ${var.lantern_box_version} pre-baked image"
}

# Alibaba Cloud ECS — build in Singapore, copy to all Asia regions.
source "alicloud-ecs" "lantern-box" {
  access_key           = var.alicloud_access_key
  secret_key           = var.alicloud_secret_key
  region               = "ap-southeast-1"
  instance_type        = "ecs.t6-c1m1.large"
  image_name           = "lantern-box-${var.lantern_box_version}-{{timestamp}}"
  image_ignore_data_disks       = true
  # Auto-discover the latest Ubuntu 24.04 base image via image_family
  # (requires alicloud plugin >= 1.1.2). This calls DescribeImageFromFamily
  # and always returns the latest system image in the family.
  image_family = "acs:ubuntu_24_04_x64"
  system_disk_mapping {
    disk_size     = 20
    disk_category = "cloud_essd"
  }
  io_optimized                  = true
  internet_charge_type          = "PayByTraffic"
  internet_max_bandwidth_out    = 5
  # Disable Alibaba's "security enhancement" (China-specific Aegis/CloudMonitor agent).
  # We run our own monitoring and don't want the extra agent on proxy servers.
  security_enhancement_strategy = "Deactive"
  force_stop_instance           = true
  ssh_username         = "root"
  ssh_password         = var.alicloud_ssh_password

  wait_copying_image_ready_timeout = 7200 # seconds (2h) — copying to 8 regions can be slow

  image_copy_regions = [
    "ap-southeast-1",  # Singapore
    "ap-southeast-3",  # Malaysia (Kuala Lumpur)
    "ap-southeast-5",  # Indonesia (Jakarta)
    "ap-southeast-6",  # Philippines (Manila)
    "ap-southeast-7",  # Thailand (Bangkok)
    "ap-northeast-1",  # Japan (Tokyo)
    "ap-northeast-2",  # South Korea (Seoul)
    "cn-hongkong",     # Hong Kong
  ]
  image_copy_names = [
    "lantern-box-${var.lantern_box_version}-{{timestamp}}",  # Singapore
    "lantern-box-${var.lantern_box_version}-{{timestamp}}",  # Malaysia
    "lantern-box-${var.lantern_box_version}-{{timestamp}}",  # Indonesia
    "lantern-box-${var.lantern_box_version}-{{timestamp}}",  # Philippines
    "lantern-box-${var.lantern_box_version}-{{timestamp}}",  # Thailand
    "lantern-box-${var.lantern_box_version}-{{timestamp}}",  # Japan
    "lantern-box-${var.lantern_box_version}-{{timestamp}}",  # South Korea
    "lantern-box-${var.lantern_box_version}-{{timestamp}}",  # Hong Kong
  ]
}

# ---------- Build ----------

build {
  sources = [
    "source.digitalocean.lantern-box",
    "source.linode.lantern-box",
    "source.oracle-oci.lantern-box-iad",
    "source.oracle-oci.lantern-box-fra",
    "source.oracle-oci.lantern-box-nrt",
    "source.oracle-oci.lantern-box-sin",
    "source.alicloud-ecs.lantern-box",
  ]

  # Upload OTel Collector config before the shell provisioner copies it into place.
  provisioner "file" {
    source      = "${path.root}/otelcol.yaml"
    destination = "/tmp/otelcol.yaml"
  }

  # Install runtime dependencies + lantern-box .deb from GitHub release
  # execute_command uses sudo for non-root SSH users (OCI uses "ubuntu")
  provisioner "shell" {
    execute_command = "sudo sh -c '{{ .Vars }} {{ .Path }}'"
    environment_vars = [
      "VERSION=${var.lantern_box_version}",
    ]
    script = "${path.root}/provision.sh"
  }

  # Clean up for smaller image and to remove any credential traces
  provisioner "shell" {
    execute_command = "sudo sh -c '{{ .Vars }} {{ .Path }}'"
    inline = [
      "apt-get clean",
      "rm -rf /var/lib/apt/lists/* /tmp/* /var/tmp/*",
      "find /var/log -type f -exec truncate -s 0 {} +",
      "rm -f /root/.bash_history /home/*/.bash_history",
      "passwd -l root",
      "sed -i 's/^PermitRootLogin.*/PermitRootLogin prohibit-password/' /etc/ssh/sshd_config",
    ]
  }

  post-processor "manifest" {
    output     = "manifest.json"
    strip_path = true
  }
}
