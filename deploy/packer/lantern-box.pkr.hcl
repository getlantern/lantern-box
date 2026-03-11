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

# OCI variables
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

variable "oci_subnet_ocid" {
  type    = string
  default = env("OCI_SUBNET_OCID")
}

variable "oci_availability_domain" {
  type    = string
  default = env("OCI_AVAILABILITY_DOMAIN")
}

variable "oci_region" {
  type    = string
  default = "us-ashburn-1"
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

# OCI API key auth — all fields must be explicit (plugin doesn't read env vars directly).
# See: https://developer.hashicorp.com/packer/integrations/hashicorp/oracle/latest/components/builder/oci
source "oracle-oci" "lantern-box" {
  tenancy_ocid        = var.oci_tenancy_ocid
  user_ocid           = var.oci_user_ocid
  fingerprint         = var.oci_fingerprint
  key_file            = var.oci_key_file
  compartment_ocid    = var.oci_compartment_ocid
  availability_domain = var.oci_availability_domain
  region              = var.oci_region

  # Ampere A1 (ARM) — cost-effective and included in OCI Always Free tier
  base_image_filter {
    display_name_search = "^Canonical-Ubuntu-24.04-Minimal-aarch64-"
    operating_system    = "Canonical Ubuntu"
  }
  shape = "VM.Standard.A1.Flex"
  shape_config {
    ocpus         = 1
    memory_in_gbs = 1
  }
  subnet_ocid  = var.oci_subnet_ocid
  ssh_username = "ubuntu"

  image_name = "lantern-box-${var.lantern_box_version}-arm64"
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

# ---------- Build ----------

build {
  sources = [
    "source.digitalocean.lantern-box",
    "source.linode.lantern-box",
    "source.oracle-oci.lantern-box",
  ]

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
    ]
  }

  post-processor "manifest" {
    output     = "manifest.json"
    strip_path = true
  }
}
