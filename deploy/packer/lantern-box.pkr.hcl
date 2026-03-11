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
  description = "Version tag to install (e.g. 1.2.3). Pulled from Gemfury .deb repo."
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

variable "fury_token" {
  type      = string
  sensitive = true
  default   = env("FURY_TOKEN")
  description = "Gemfury token for accessing the private .deb repo."
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
variable "oci_compartment_ocid" {
  type        = string
  default     = env("OCI_COMPARTMENT_OCID")
  description = "OCI compartment OCID where the image will be created."
}

variable "oci_subnet_ocid" {
  type        = string
  default     = env("OCI_SUBNET_OCID")
  description = "OCI subnet OCID for the build instance."
}

variable "oci_availability_domain" {
  type        = string
  default     = env("OCI_AVAILABILITY_DOMAIN")
  description = "OCI availability domain (e.g. TYgz:US-ASHBURN-AD-1)."
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
}

# OCI uses API key auth via ~/.oci/config or environment variables.
# See: https://developer.hashicorp.com/packer/integrations/hashicorp/oracle/latest/components/builder/oci
source "oracle-oci" "lantern-box" {
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
    memory_in_gbs = 6
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

  # Install runtime dependencies + lantern-box from Gemfury .deb repo
  provisioner "shell" {
    environment_vars = [
      "FURY_TOKEN=${var.fury_token}",
    ]
    script = "${path.root}/provision.sh"
  }

  # Clean up for smaller image and to remove any credential traces
  provisioner "shell" {
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
