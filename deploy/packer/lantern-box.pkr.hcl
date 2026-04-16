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

# Shared OCI config used by all per-region source blocks. Centralizing these
# values avoids error-prone repetition across 36 regions.
locals {
  oci_base_image_regex = "^Canonical-Ubuntu-24.04-Minimal-aarch64-"
  oci_base_image_os    = "Canonical Ubuntu"
  oci_shape            = "VM.Standard.A1.Flex"
  oci_ocpus            = 1
  oci_memory_gbs       = 1
  oci_ssh_username     = "ubuntu"
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
    display_name_search = local.oci_base_image_regex
    operating_system    = local.oci_base_image_os
  }
  shape = local.oci_shape
  shape_config {
    ocpus         = local.oci_ocpus
    memory_in_gbs = local.oci_memory_gbs
  }
  subnet_ocid  = var.oci_subnet_ocid_iad
  ssh_username = local.oci_ssh_username

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
    display_name_search = local.oci_base_image_regex
    operating_system    = local.oci_base_image_os
  }
  shape = local.oci_shape
  shape_config {
    ocpus         = local.oci_ocpus
    memory_in_gbs = local.oci_memory_gbs
  }
  subnet_ocid  = var.oci_subnet_ocid_fra
  ssh_username = local.oci_ssh_username

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
    display_name_search = local.oci_base_image_regex
    operating_system    = local.oci_base_image_os
  }
  shape = local.oci_shape
  shape_config {
    ocpus         = local.oci_ocpus
    memory_in_gbs = local.oci_memory_gbs
  }
  subnet_ocid  = var.oci_subnet_ocid_nrt
  ssh_username = local.oci_ssh_username

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
    display_name_search = local.oci_base_image_regex
    operating_system    = local.oci_base_image_os
  }
  shape = local.oci_shape
  shape_config {
    ocpus         = local.oci_ocpus
    memory_in_gbs = local.oci_memory_gbs
  }
  subnet_ocid  = var.oci_subnet_ocid_sin
  ssh_username = local.oci_ssh_username

  image_name = "lantern-box-${var.lantern_box_version}-arm64-sin"
}

source "oracle-oci" "lantern-box-phx" {
  tenancy_ocid        = var.oci_tenancy_ocid
  user_ocid           = var.oci_user_ocid
  fingerprint         = var.oci_fingerprint
  key_file            = var.oci_key_file
  compartment_ocid    = var.oci_compartment_ocid
  availability_domain = var.oci_availability_domain_phx
  region              = "us-phoenix-1"

  base_image_filter {
    display_name_search = local.oci_base_image_regex
    operating_system    = local.oci_base_image_os
  }
  shape = local.oci_shape
  shape_config {
    ocpus         = local.oci_ocpus
    memory_in_gbs = local.oci_memory_gbs
  }
  subnet_ocid  = var.oci_subnet_ocid_phx
  ssh_username = local.oci_ssh_username

  image_name = "lantern-box-${var.lantern_box_version}-arm64-phx"
}

source "oracle-oci" "lantern-box-ams" {
  tenancy_ocid        = var.oci_tenancy_ocid
  user_ocid           = var.oci_user_ocid
  fingerprint         = var.oci_fingerprint
  key_file            = var.oci_key_file
  compartment_ocid    = var.oci_compartment_ocid
  availability_domain = var.oci_availability_domain_ams
  region              = "eu-amsterdam-1"

  base_image_filter {
    display_name_search = local.oci_base_image_regex
    operating_system    = local.oci_base_image_os
  }
  shape = local.oci_shape
  shape_config {
    ocpus         = local.oci_ocpus
    memory_in_gbs = local.oci_memory_gbs
  }
  subnet_ocid  = var.oci_subnet_ocid_ams
  ssh_username = local.oci_ssh_username

  image_name = "lantern-box-${var.lantern_box_version}-arm64-ams"
}

source "oracle-oci" "lantern-box-bom" {
  tenancy_ocid        = var.oci_tenancy_ocid
  user_ocid           = var.oci_user_ocid
  fingerprint         = var.oci_fingerprint
  key_file            = var.oci_key_file
  compartment_ocid    = var.oci_compartment_ocid
  availability_domain = var.oci_availability_domain_bom
  region              = "ap-mumbai-1"

  base_image_filter {
    display_name_search = local.oci_base_image_regex
    operating_system    = local.oci_base_image_os
  }
  shape = local.oci_shape
  shape_config {
    ocpus         = local.oci_ocpus
    memory_in_gbs = local.oci_memory_gbs
  }
  subnet_ocid  = var.oci_subnet_ocid_bom
  ssh_username = local.oci_ssh_username

  image_name = "lantern-box-${var.lantern_box_version}-arm64-bom"
}

source "oracle-oci" "lantern-box-gru" {
  tenancy_ocid        = var.oci_tenancy_ocid
  user_ocid           = var.oci_user_ocid
  fingerprint         = var.oci_fingerprint
  key_file            = var.oci_key_file
  compartment_ocid    = var.oci_compartment_ocid
  availability_domain = var.oci_availability_domain_gru
  region              = "sa-saopaulo-1"

  base_image_filter {
    display_name_search = local.oci_base_image_regex
    operating_system    = local.oci_base_image_os
  }
  shape = local.oci_shape
  shape_config {
    ocpus         = local.oci_ocpus
    memory_in_gbs = local.oci_memory_gbs
  }
  subnet_ocid  = var.oci_subnet_ocid_gru
  ssh_username = local.oci_ssh_username

  image_name = "lantern-box-${var.lantern_box_version}-arm64-gru"
}

# ---------- North America (new) ----------------------------------------

source "oracle-oci" "lantern-box-ord" {
  tenancy_ocid        = var.oci_tenancy_ocid
  user_ocid           = var.oci_user_ocid
  fingerprint         = var.oci_fingerprint
  key_file            = var.oci_key_file
  compartment_ocid    = var.oci_compartment_ocid
  availability_domain = var.oci_availability_domain_ord
  region              = "us-chicago-1"

  base_image_filter {
    display_name_search = local.oci_base_image_regex
    operating_system    = local.oci_base_image_os
  }
  shape = local.oci_shape
  shape_config {
    ocpus         = local.oci_ocpus
    memory_in_gbs = local.oci_memory_gbs
  }
  subnet_ocid  = var.oci_subnet_ocid_ord
  ssh_username = local.oci_ssh_username

  image_name = "lantern-box-${var.lantern_box_version}-arm64-ord"
}

source "oracle-oci" "lantern-box-sjc" {
  tenancy_ocid        = var.oci_tenancy_ocid
  user_ocid           = var.oci_user_ocid
  fingerprint         = var.oci_fingerprint
  key_file            = var.oci_key_file
  compartment_ocid    = var.oci_compartment_ocid
  availability_domain = var.oci_availability_domain_sjc
  region              = "us-sanjose-1"

  base_image_filter {
    display_name_search = local.oci_base_image_regex
    operating_system    = local.oci_base_image_os
  }
  shape = local.oci_shape
  shape_config {
    ocpus         = local.oci_ocpus
    memory_in_gbs = local.oci_memory_gbs
  }
  subnet_ocid  = var.oci_subnet_ocid_sjc
  ssh_username = local.oci_ssh_username

  image_name = "lantern-box-${var.lantern_box_version}-arm64-sjc"
}

source "oracle-oci" "lantern-box-yyz" {
  tenancy_ocid        = var.oci_tenancy_ocid
  user_ocid           = var.oci_user_ocid
  fingerprint         = var.oci_fingerprint
  key_file            = var.oci_key_file
  compartment_ocid    = var.oci_compartment_ocid
  availability_domain = var.oci_availability_domain_yyz
  region              = "ca-toronto-1"

  base_image_filter {
    display_name_search = local.oci_base_image_regex
    operating_system    = local.oci_base_image_os
  }
  shape = local.oci_shape
  shape_config {
    ocpus         = local.oci_ocpus
    memory_in_gbs = local.oci_memory_gbs
  }
  subnet_ocid  = var.oci_subnet_ocid_yyz
  ssh_username = local.oci_ssh_username

  image_name = "lantern-box-${var.lantern_box_version}-arm64-yyz"
}

source "oracle-oci" "lantern-box-yul" {
  tenancy_ocid        = var.oci_tenancy_ocid
  user_ocid           = var.oci_user_ocid
  fingerprint         = var.oci_fingerprint
  key_file            = var.oci_key_file
  compartment_ocid    = var.oci_compartment_ocid
  availability_domain = var.oci_availability_domain_yul
  region              = "ca-montreal-1"

  base_image_filter {
    display_name_search = local.oci_base_image_regex
    operating_system    = local.oci_base_image_os
  }
  shape = local.oci_shape
  shape_config {
    ocpus         = local.oci_ocpus
    memory_in_gbs = local.oci_memory_gbs
  }
  subnet_ocid  = var.oci_subnet_ocid_yul
  ssh_username = local.oci_ssh_username

  image_name = "lantern-box-${var.lantern_box_version}-arm64-yul"
}

source "oracle-oci" "lantern-box-mty" {
  tenancy_ocid        = var.oci_tenancy_ocid
  user_ocid           = var.oci_user_ocid
  fingerprint         = var.oci_fingerprint
  key_file            = var.oci_key_file
  compartment_ocid    = var.oci_compartment_ocid
  availability_domain = var.oci_availability_domain_mty
  region              = "mx-monterrey-1"

  base_image_filter {
    display_name_search = local.oci_base_image_regex
    operating_system    = local.oci_base_image_os
  }
  shape = local.oci_shape
  shape_config {
    ocpus         = local.oci_ocpus
    memory_in_gbs = local.oci_memory_gbs
  }
  subnet_ocid  = var.oci_subnet_ocid_mty
  ssh_username = local.oci_ssh_username

  image_name = "lantern-box-${var.lantern_box_version}-arm64-mty"
}

source "oracle-oci" "lantern-box-qro" {
  tenancy_ocid        = var.oci_tenancy_ocid
  user_ocid           = var.oci_user_ocid
  fingerprint         = var.oci_fingerprint
  key_file            = var.oci_key_file
  compartment_ocid    = var.oci_compartment_ocid
  availability_domain = var.oci_availability_domain_qro
  region              = "mx-queretaro-1"

  base_image_filter {
    display_name_search = local.oci_base_image_regex
    operating_system    = local.oci_base_image_os
  }
  shape = local.oci_shape
  shape_config {
    ocpus         = local.oci_ocpus
    memory_in_gbs = local.oci_memory_gbs
  }
  subnet_ocid  = var.oci_subnet_ocid_qro
  ssh_username = local.oci_ssh_username

  image_name = "lantern-box-${var.lantern_box_version}-arm64-qro"
}

# ---------- Europe (new) -----------------------------------------------

source "oracle-oci" "lantern-box-mrs" {
  tenancy_ocid        = var.oci_tenancy_ocid
  user_ocid           = var.oci_user_ocid
  fingerprint         = var.oci_fingerprint
  key_file            = var.oci_key_file
  compartment_ocid    = var.oci_compartment_ocid
  availability_domain = var.oci_availability_domain_mrs
  region              = "eu-marseille-1"

  base_image_filter {
    display_name_search = local.oci_base_image_regex
    operating_system    = local.oci_base_image_os
  }
  shape = local.oci_shape
  shape_config {
    ocpus         = local.oci_ocpus
    memory_in_gbs = local.oci_memory_gbs
  }
  subnet_ocid  = var.oci_subnet_ocid_mrs
  ssh_username = local.oci_ssh_username

  image_name = "lantern-box-${var.lantern_box_version}-arm64-mrs"
}

source "oracle-oci" "lantern-box-lin" {
  tenancy_ocid        = var.oci_tenancy_ocid
  user_ocid           = var.oci_user_ocid
  fingerprint         = var.oci_fingerprint
  key_file            = var.oci_key_file
  compartment_ocid    = var.oci_compartment_ocid
  availability_domain = var.oci_availability_domain_lin
  region              = "eu-milan-1"

  base_image_filter {
    display_name_search = local.oci_base_image_regex
    operating_system    = local.oci_base_image_os
  }
  shape = local.oci_shape
  shape_config {
    ocpus         = local.oci_ocpus
    memory_in_gbs = local.oci_memory_gbs
  }
  subnet_ocid  = var.oci_subnet_ocid_lin
  ssh_username = local.oci_ssh_username

  image_name = "lantern-box-${var.lantern_box_version}-arm64-lin"
}

source "oracle-oci" "lantern-box-mad" {
  tenancy_ocid        = var.oci_tenancy_ocid
  user_ocid           = var.oci_user_ocid
  fingerprint         = var.oci_fingerprint
  key_file            = var.oci_key_file
  compartment_ocid    = var.oci_compartment_ocid
  availability_domain = var.oci_availability_domain_mad
  region              = "eu-madrid-1"

  base_image_filter {
    display_name_search = local.oci_base_image_regex
    operating_system    = local.oci_base_image_os
  }
  shape = local.oci_shape
  shape_config {
    ocpus         = local.oci_ocpus
    memory_in_gbs = local.oci_memory_gbs
  }
  subnet_ocid  = var.oci_subnet_ocid_mad
  ssh_username = local.oci_ssh_username

  image_name = "lantern-box-${var.lantern_box_version}-arm64-mad"
}

source "oracle-oci" "lantern-box-arn" {
  tenancy_ocid        = var.oci_tenancy_ocid
  user_ocid           = var.oci_user_ocid
  fingerprint         = var.oci_fingerprint
  key_file            = var.oci_key_file
  compartment_ocid    = var.oci_compartment_ocid
  availability_domain = var.oci_availability_domain_arn
  region              = "eu-stockholm-1"

  base_image_filter {
    display_name_search = local.oci_base_image_regex
    operating_system    = local.oci_base_image_os
  }
  shape = local.oci_shape
  shape_config {
    ocpus         = local.oci_ocpus
    memory_in_gbs = local.oci_memory_gbs
  }
  subnet_ocid  = var.oci_subnet_ocid_arn
  ssh_username = local.oci_ssh_username

  image_name = "lantern-box-${var.lantern_box_version}-arm64-arn"
}

source "oracle-oci" "lantern-box-zrh" {
  tenancy_ocid        = var.oci_tenancy_ocid
  user_ocid           = var.oci_user_ocid
  fingerprint         = var.oci_fingerprint
  key_file            = var.oci_key_file
  compartment_ocid    = var.oci_compartment_ocid
  availability_domain = var.oci_availability_domain_zrh
  region              = "eu-zurich-1"

  base_image_filter {
    display_name_search = local.oci_base_image_regex
    operating_system    = local.oci_base_image_os
  }
  shape = local.oci_shape
  shape_config {
    ocpus         = local.oci_ocpus
    memory_in_gbs = local.oci_memory_gbs
  }
  subnet_ocid  = var.oci_subnet_ocid_zrh
  ssh_username = local.oci_ssh_username

  image_name = "lantern-box-${var.lantern_box_version}-arm64-zrh"
}

source "oracle-oci" "lantern-box-cdg" {
  tenancy_ocid        = var.oci_tenancy_ocid
  user_ocid           = var.oci_user_ocid
  fingerprint         = var.oci_fingerprint
  key_file            = var.oci_key_file
  compartment_ocid    = var.oci_compartment_ocid
  availability_domain = var.oci_availability_domain_cdg
  region              = "eu-paris-1"

  base_image_filter {
    display_name_search = local.oci_base_image_regex
    operating_system    = local.oci_base_image_os
  }
  shape = local.oci_shape
  shape_config {
    ocpus         = local.oci_ocpus
    memory_in_gbs = local.oci_memory_gbs
  }
  subnet_ocid  = var.oci_subnet_ocid_cdg
  ssh_username = local.oci_ssh_username

  image_name = "lantern-box-${var.lantern_box_version}-arm64-cdg"
}

source "oracle-oci" "lantern-box-lhr" {
  tenancy_ocid        = var.oci_tenancy_ocid
  user_ocid           = var.oci_user_ocid
  fingerprint         = var.oci_fingerprint
  key_file            = var.oci_key_file
  compartment_ocid    = var.oci_compartment_ocid
  availability_domain = var.oci_availability_domain_lhr
  region              = "uk-london-1"

  base_image_filter {
    display_name_search = local.oci_base_image_regex
    operating_system    = local.oci_base_image_os
  }
  shape = local.oci_shape
  shape_config {
    ocpus         = local.oci_ocpus
    memory_in_gbs = local.oci_memory_gbs
  }
  subnet_ocid  = var.oci_subnet_ocid_lhr
  ssh_username = local.oci_ssh_username

  image_name = "lantern-box-${var.lantern_box_version}-arm64-lhr"
}

source "oracle-oci" "lantern-box-cwl" {
  tenancy_ocid        = var.oci_tenancy_ocid
  user_ocid           = var.oci_user_ocid
  fingerprint         = var.oci_fingerprint
  key_file            = var.oci_key_file
  compartment_ocid    = var.oci_compartment_ocid
  availability_domain = var.oci_availability_domain_cwl
  region              = "uk-cardiff-1"

  base_image_filter {
    display_name_search = local.oci_base_image_regex
    operating_system    = local.oci_base_image_os
  }
  shape = local.oci_shape
  shape_config {
    ocpus         = local.oci_ocpus
    memory_in_gbs = local.oci_memory_gbs
  }
  subnet_ocid  = var.oci_subnet_ocid_cwl
  ssh_username = local.oci_ssh_username

  image_name = "lantern-box-${var.lantern_box_version}-arm64-cwl"
}

# ---------- Asia-Pacific (new) -----------------------------------------

source "oracle-oci" "lantern-box-icn" {
  tenancy_ocid        = var.oci_tenancy_ocid
  user_ocid           = var.oci_user_ocid
  fingerprint         = var.oci_fingerprint
  key_file            = var.oci_key_file
  compartment_ocid    = var.oci_compartment_ocid
  availability_domain = var.oci_availability_domain_icn
  region              = "ap-seoul-1"

  base_image_filter {
    display_name_search = local.oci_base_image_regex
    operating_system    = local.oci_base_image_os
  }
  shape = local.oci_shape
  shape_config {
    ocpus         = local.oci_ocpus
    memory_in_gbs = local.oci_memory_gbs
  }
  subnet_ocid  = var.oci_subnet_ocid_icn
  ssh_username = local.oci_ssh_username

  image_name = "lantern-box-${var.lantern_box_version}-arm64-icn"
}

source "oracle-oci" "lantern-box-kix" {
  tenancy_ocid        = var.oci_tenancy_ocid
  user_ocid           = var.oci_user_ocid
  fingerprint         = var.oci_fingerprint
  key_file            = var.oci_key_file
  compartment_ocid    = var.oci_compartment_ocid
  availability_domain = var.oci_availability_domain_kix
  region              = "ap-osaka-1"

  base_image_filter {
    display_name_search = local.oci_base_image_regex
    operating_system    = local.oci_base_image_os
  }
  shape = local.oci_shape
  shape_config {
    ocpus         = local.oci_ocpus
    memory_in_gbs = local.oci_memory_gbs
  }
  subnet_ocid  = var.oci_subnet_ocid_kix
  ssh_username = local.oci_ssh_username

  image_name = "lantern-box-${var.lantern_box_version}-arm64-kix"
}

source "oracle-oci" "lantern-box-mel" {
  tenancy_ocid        = var.oci_tenancy_ocid
  user_ocid           = var.oci_user_ocid
  fingerprint         = var.oci_fingerprint
  key_file            = var.oci_key_file
  compartment_ocid    = var.oci_compartment_ocid
  availability_domain = var.oci_availability_domain_mel
  region              = "ap-melbourne-1"

  base_image_filter {
    display_name_search = local.oci_base_image_regex
    operating_system    = local.oci_base_image_os
  }
  shape = local.oci_shape
  shape_config {
    ocpus         = local.oci_ocpus
    memory_in_gbs = local.oci_memory_gbs
  }
  subnet_ocid  = var.oci_subnet_ocid_mel
  ssh_username = local.oci_ssh_username

  image_name = "lantern-box-${var.lantern_box_version}-arm64-mel"
}

source "oracle-oci" "lantern-box-syd" {
  tenancy_ocid        = var.oci_tenancy_ocid
  user_ocid           = var.oci_user_ocid
  fingerprint         = var.oci_fingerprint
  key_file            = var.oci_key_file
  compartment_ocid    = var.oci_compartment_ocid
  availability_domain = var.oci_availability_domain_syd
  region              = "ap-sydney-1"

  base_image_filter {
    display_name_search = local.oci_base_image_regex
    operating_system    = local.oci_base_image_os
  }
  shape = local.oci_shape
  shape_config {
    ocpus         = local.oci_ocpus
    memory_in_gbs = local.oci_memory_gbs
  }
  subnet_ocid  = var.oci_subnet_ocid_syd
  ssh_username = local.oci_ssh_username

  image_name = "lantern-box-${var.lantern_box_version}-arm64-syd"
}

source "oracle-oci" "lantern-box-ynj" {
  tenancy_ocid        = var.oci_tenancy_ocid
  user_ocid           = var.oci_user_ocid
  fingerprint         = var.oci_fingerprint
  key_file            = var.oci_key_file
  compartment_ocid    = var.oci_compartment_ocid
  availability_domain = var.oci_availability_domain_ynj
  region              = "ap-chuncheon-1"

  base_image_filter {
    display_name_search = local.oci_base_image_regex
    operating_system    = local.oci_base_image_os
  }
  shape = local.oci_shape
  shape_config {
    ocpus         = local.oci_ocpus
    memory_in_gbs = local.oci_memory_gbs
  }
  subnet_ocid  = var.oci_subnet_ocid_ynj
  ssh_username = local.oci_ssh_username

  image_name = "lantern-box-${var.lantern_box_version}-arm64-ynj"
}

source "oracle-oci" "lantern-box-xsp" {
  tenancy_ocid        = var.oci_tenancy_ocid
  user_ocid           = var.oci_user_ocid
  fingerprint         = var.oci_fingerprint
  key_file            = var.oci_key_file
  compartment_ocid    = var.oci_compartment_ocid
  availability_domain = var.oci_availability_domain_xsp
  region              = "ap-singapore-2"

  base_image_filter {
    display_name_search = local.oci_base_image_regex
    operating_system    = local.oci_base_image_os
  }
  shape = local.oci_shape
  shape_config {
    ocpus         = local.oci_ocpus
    memory_in_gbs = local.oci_memory_gbs
  }
  subnet_ocid  = var.oci_subnet_ocid_xsp
  ssh_username = local.oci_ssh_username

  image_name = "lantern-box-${var.lantern_box_version}-arm64-xsp"
}

# ---------- Middle East / Africa (new) ----------------------------------

source "oracle-oci" "lantern-box-dxb" {
  tenancy_ocid        = var.oci_tenancy_ocid
  user_ocid           = var.oci_user_ocid
  fingerprint         = var.oci_fingerprint
  key_file            = var.oci_key_file
  compartment_ocid    = var.oci_compartment_ocid
  availability_domain = var.oci_availability_domain_dxb
  region              = "me-dubai-1"

  base_image_filter {
    display_name_search = local.oci_base_image_regex
    operating_system    = local.oci_base_image_os
  }
  shape = local.oci_shape
  shape_config {
    ocpus         = local.oci_ocpus
    memory_in_gbs = local.oci_memory_gbs
  }
  subnet_ocid  = var.oci_subnet_ocid_dxb
  ssh_username = local.oci_ssh_username

  image_name = "lantern-box-${var.lantern_box_version}-arm64-dxb"
}

source "oracle-oci" "lantern-box-jed" {
  tenancy_ocid        = var.oci_tenancy_ocid
  user_ocid           = var.oci_user_ocid
  fingerprint         = var.oci_fingerprint
  key_file            = var.oci_key_file
  compartment_ocid    = var.oci_compartment_ocid
  availability_domain = var.oci_availability_domain_jed
  region              = "me-jeddah-1"

  base_image_filter {
    display_name_search = local.oci_base_image_regex
    operating_system    = local.oci_base_image_os
  }
  shape = local.oci_shape
  shape_config {
    ocpus         = local.oci_ocpus
    memory_in_gbs = local.oci_memory_gbs
  }
  subnet_ocid  = var.oci_subnet_ocid_jed
  ssh_username = local.oci_ssh_username

  image_name = "lantern-box-${var.lantern_box_version}-arm64-jed"
}

source "oracle-oci" "lantern-box-ruh" {
  tenancy_ocid        = var.oci_tenancy_ocid
  user_ocid           = var.oci_user_ocid
  fingerprint         = var.oci_fingerprint
  key_file            = var.oci_key_file
  compartment_ocid    = var.oci_compartment_ocid
  availability_domain = var.oci_availability_domain_ruh
  region              = "me-riyadh-1"

  base_image_filter {
    display_name_search = local.oci_base_image_regex
    operating_system    = local.oci_base_image_os
  }
  shape = local.oci_shape
  shape_config {
    ocpus         = local.oci_ocpus
    memory_in_gbs = local.oci_memory_gbs
  }
  subnet_ocid  = var.oci_subnet_ocid_ruh
  ssh_username = local.oci_ssh_username

  image_name = "lantern-box-${var.lantern_box_version}-arm64-ruh"
}

source "oracle-oci" "lantern-box-jrs" {
  tenancy_ocid        = var.oci_tenancy_ocid
  user_ocid           = var.oci_user_ocid
  fingerprint         = var.oci_fingerprint
  key_file            = var.oci_key_file
  compartment_ocid    = var.oci_compartment_ocid
  availability_domain = var.oci_availability_domain_jrs
  region              = "il-jerusalem-1"

  base_image_filter {
    display_name_search = local.oci_base_image_regex
    operating_system    = local.oci_base_image_os
  }
  shape = local.oci_shape
  shape_config {
    ocpus         = local.oci_ocpus
    memory_in_gbs = local.oci_memory_gbs
  }
  subnet_ocid  = var.oci_subnet_ocid_jrs
  ssh_username = local.oci_ssh_username

  image_name = "lantern-box-${var.lantern_box_version}-arm64-jrs"
}

source "oracle-oci" "lantern-box-jnb" {
  tenancy_ocid        = var.oci_tenancy_ocid
  user_ocid           = var.oci_user_ocid
  fingerprint         = var.oci_fingerprint
  key_file            = var.oci_key_file
  compartment_ocid    = var.oci_compartment_ocid
  availability_domain = var.oci_availability_domain_jnb
  region              = "af-johannesburg-1"

  base_image_filter {
    display_name_search = local.oci_base_image_regex
    operating_system    = local.oci_base_image_os
  }
  shape = local.oci_shape
  shape_config {
    ocpus         = local.oci_ocpus
    memory_in_gbs = local.oci_memory_gbs
  }
  subnet_ocid  = var.oci_subnet_ocid_jnb
  ssh_username = local.oci_ssh_username

  image_name = "lantern-box-${var.lantern_box_version}-arm64-jnb"
}

# ---------- Latin America (new) -----------------------------------------

source "oracle-oci" "lantern-box-scl" {
  tenancy_ocid        = var.oci_tenancy_ocid
  user_ocid           = var.oci_user_ocid
  fingerprint         = var.oci_fingerprint
  key_file            = var.oci_key_file
  compartment_ocid    = var.oci_compartment_ocid
  availability_domain = var.oci_availability_domain_scl
  region              = "sa-santiago-1"

  base_image_filter {
    display_name_search = local.oci_base_image_regex
    operating_system    = local.oci_base_image_os
  }
  shape = local.oci_shape
  shape_config {
    ocpus         = local.oci_ocpus
    memory_in_gbs = local.oci_memory_gbs
  }
  subnet_ocid  = var.oci_subnet_ocid_scl
  ssh_username = local.oci_ssh_username

  image_name = "lantern-box-${var.lantern_box_version}-arm64-scl"
}

source "oracle-oci" "lantern-box-bog" {
  tenancy_ocid        = var.oci_tenancy_ocid
  user_ocid           = var.oci_user_ocid
  fingerprint         = var.oci_fingerprint
  key_file            = var.oci_key_file
  compartment_ocid    = var.oci_compartment_ocid
  availability_domain = var.oci_availability_domain_bog
  region              = "sa-bogota-1"

  base_image_filter {
    display_name_search = local.oci_base_image_regex
    operating_system    = local.oci_base_image_os
  }
  shape = local.oci_shape
  shape_config {
    ocpus         = local.oci_ocpus
    memory_in_gbs = local.oci_memory_gbs
  }
  subnet_ocid  = var.oci_subnet_ocid_bog
  ssh_username = local.oci_ssh_username

  image_name = "lantern-box-${var.lantern_box_version}-arm64-bog"
}

source "oracle-oci" "lantern-box-vap" {
  tenancy_ocid        = var.oci_tenancy_ocid
  user_ocid           = var.oci_user_ocid
  fingerprint         = var.oci_fingerprint
  key_file            = var.oci_key_file
  compartment_ocid    = var.oci_compartment_ocid
  availability_domain = var.oci_availability_domain_vap
  region              = "sa-valparaiso-1"

  base_image_filter {
    display_name_search = local.oci_base_image_regex
    operating_system    = local.oci_base_image_os
  }
  shape = local.oci_shape
  shape_config {
    ocpus         = local.oci_ocpus
    memory_in_gbs = local.oci_memory_gbs
  }
  subnet_ocid  = var.oci_subnet_ocid_vap
  ssh_username = local.oci_ssh_username

  image_name = "lantern-box-${var.lantern_box_version}-arm64-vap"
}

source "linode" "lantern-box" {
  linode_token  = var.linode_token
  image         = "linode/ubuntu24.04"
  region        = var.linode_region
  instance_type = "g6-nanode-1"
  ssh_username  = "root"
  image_label   = "lantern-box-${var.lantern_box_version}"
  image_description = "lantern-box ${var.lantern_box_version} pre-baked image"

  # Replicate to Linode regions that support image replication.
  # The build region (var.linode_region) must be included or it will be removed.
  # Excluded: legacy IDs (us-west, ap-southeast, etc.) and newer regions that
  # don't yet support replication (gb-lon, de-fra-2, fr-par-2, in-bom-2,
  # sg-sin-2, jp-tyo-3).
  image_regions = [
    "us-lax", "us-mia", "us-sea", "us-ord", "us-iad",
    "us-east", "us-southeast",
    "fr-par", "nl-ams", "it-mil", "es-mad", "se-sto",
    "in-maa", "jp-osa", "au-mel",
    "id-cgk", "br-gru",
  ]
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
    "source.oracle-oci.lantern-box-phx",
    "source.oracle-oci.lantern-box-ams",
    "source.oracle-oci.lantern-box-bom",
    "source.oracle-oci.lantern-box-gru",
    # North America (new)
    "source.oracle-oci.lantern-box-ord",
    "source.oracle-oci.lantern-box-sjc",
    "source.oracle-oci.lantern-box-yyz",
    "source.oracle-oci.lantern-box-yul",
    "source.oracle-oci.lantern-box-mty",
    "source.oracle-oci.lantern-box-qro",
    # Europe (new)
    "source.oracle-oci.lantern-box-mrs",
    "source.oracle-oci.lantern-box-lin",
    "source.oracle-oci.lantern-box-mad",
    "source.oracle-oci.lantern-box-arn",
    "source.oracle-oci.lantern-box-zrh",
    "source.oracle-oci.lantern-box-cdg",
    "source.oracle-oci.lantern-box-lhr",
    "source.oracle-oci.lantern-box-cwl",
    # Asia-Pacific (new)
    "source.oracle-oci.lantern-box-icn",
    "source.oracle-oci.lantern-box-kix",
    "source.oracle-oci.lantern-box-mel",
    "source.oracle-oci.lantern-box-syd",
    "source.oracle-oci.lantern-box-ynj",
    "source.oracle-oci.lantern-box-xsp",
    # Middle East / Africa (new)
    "source.oracle-oci.lantern-box-dxb",
    "source.oracle-oci.lantern-box-jed",
    "source.oracle-oci.lantern-box-ruh",
    "source.oracle-oci.lantern-box-jrs",
    "source.oracle-oci.lantern-box-jnb",
    # Latin America (new)
    "source.oracle-oci.lantern-box-scl",
    "source.oracle-oci.lantern-box-bog",
    "source.oracle-oci.lantern-box-vap",
    "source.alicloud-ecs.lantern-box",
  ]

  # Ensure datacap placeholder files exist so the file provisioner never fails.
  # In CI, the real binaries are downloaded from the build-datacap job artifact.
  # For local/OSS builds, these will be empty (and skipped at install time).
  provisioner "shell-local" {
    inline = [
      "touch /tmp/datacap-amd64 /tmp/datacap-arm64",
    ]
  }

  # Upload the datacap binary for this arch.
  # OCI sources target arm64; DigitalOcean/Linode/Alicloud target amd64.
  provisioner "file" {
    only        = ["digitalocean.lantern-box", "linode.lantern-box", "alicloud-ecs.lantern-box"]
    source      = "/tmp/datacap-amd64"
    destination = "/tmp/datacap"
  }

  provisioner "file" {
    except      = ["digitalocean.lantern-box", "linode.lantern-box", "alicloud-ecs.lantern-box"]
    source      = "/tmp/datacap-arm64"
    destination = "/tmp/datacap"
  }

  provisioner "shell" {
    execute_command = "sudo sh -c '{{ .Vars }} {{ .Path }}'"
    inline = [
      <<-EOF
      if [ -s /tmp/datacap ]; then
        install -o root -g root -m 755 /tmp/datacap /usr/local/bin/datacap
        rm -f /tmp/datacap
        echo "datacap installed to /usr/local/bin/datacap"
      else
        rm -f /tmp/datacap
        echo "datacap binary not staged, skipping"
      fi
      EOF
    ]
  }

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
