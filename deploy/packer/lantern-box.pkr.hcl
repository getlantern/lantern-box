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
  default = "us-lax"
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

# OCI per-region variables: us-phoenix-1 (PHX)
variable "oci_subnet_ocid_phx" {
  type    = string
  default = env("OCI_SUBNET_OCID_PHX")
}

variable "oci_availability_domain_phx" {
  type    = string
  default = env("OCI_AVAILABILITY_DOMAIN_PHX")
}

# OCI per-region variables: eu-amsterdam-1 (AMS)
variable "oci_subnet_ocid_ams" {
  type    = string
  default = env("OCI_SUBNET_OCID_AMS")
}

variable "oci_availability_domain_ams" {
  type    = string
  default = env("OCI_AVAILABILITY_DOMAIN_AMS")
}

# OCI per-region variables: ap-mumbai-1 (BOM)
variable "oci_subnet_ocid_bom" {
  type    = string
  default = env("OCI_SUBNET_OCID_BOM")
}

variable "oci_availability_domain_bom" {
  type    = string
  default = env("OCI_AVAILABILITY_DOMAIN_BOM")
}

# OCI per-region variables: sa-saopaulo-1 (GRU)
variable "oci_subnet_ocid_gru" {
  type    = string
  default = env("OCI_SUBNET_OCID_GRU")
}

variable "oci_availability_domain_gru" {
  type    = string
  default = env("OCI_AVAILABILITY_DOMAIN_GRU")
}

# ── North America (new) ────────────────────────────────────────────────

# OCI per-region variables: us-chicago-1 (ORD)
variable "oci_subnet_ocid_ord" {
  type    = string
  default = env("OCI_SUBNET_OCID_ORD")
}

variable "oci_availability_domain_ord" {
  type    = string
  default = env("OCI_AVAILABILITY_DOMAIN_ORD")
}

# OCI per-region variables: us-sanjose-1 (SJC)
variable "oci_subnet_ocid_sjc" {
  type    = string
  default = env("OCI_SUBNET_OCID_SJC")
}

variable "oci_availability_domain_sjc" {
  type    = string
  default = env("OCI_AVAILABILITY_DOMAIN_SJC")
}

# OCI per-region variables: ca-toronto-1 (YYZ)
variable "oci_subnet_ocid_yyz" {
  type    = string
  default = env("OCI_SUBNET_OCID_YYZ")
}

variable "oci_availability_domain_yyz" {
  type    = string
  default = env("OCI_AVAILABILITY_DOMAIN_YYZ")
}

# OCI per-region variables: ca-montreal-1 (YUL)
variable "oci_subnet_ocid_yul" {
  type    = string
  default = env("OCI_SUBNET_OCID_YUL")
}

variable "oci_availability_domain_yul" {
  type    = string
  default = env("OCI_AVAILABILITY_DOMAIN_YUL")
}

# OCI per-region variables: mx-monterrey-1 (MTY)
variable "oci_subnet_ocid_mty" {
  type    = string
  default = env("OCI_SUBNET_OCID_MTY")
}

variable "oci_availability_domain_mty" {
  type    = string
  default = env("OCI_AVAILABILITY_DOMAIN_MTY")
}

# OCI per-region variables: mx-queretaro-1 (QRO)
variable "oci_subnet_ocid_qro" {
  type    = string
  default = env("OCI_SUBNET_OCID_QRO")
}

variable "oci_availability_domain_qro" {
  type    = string
  default = env("OCI_AVAILABILITY_DOMAIN_QRO")
}

# ── Europe (new) ───────────────────────────────────────────────────────

# OCI per-region variables: eu-marseille-1 (MRS)
variable "oci_subnet_ocid_mrs" {
  type    = string
  default = env("OCI_SUBNET_OCID_MRS")
}

variable "oci_availability_domain_mrs" {
  type    = string
  default = env("OCI_AVAILABILITY_DOMAIN_MRS")
}

# OCI per-region variables: eu-milan-1 (LIN)
variable "oci_subnet_ocid_lin" {
  type    = string
  default = env("OCI_SUBNET_OCID_LIN")
}

variable "oci_availability_domain_lin" {
  type    = string
  default = env("OCI_AVAILABILITY_DOMAIN_LIN")
}

# OCI per-region variables: eu-madrid-1 (MAD)
variable "oci_subnet_ocid_mad" {
  type    = string
  default = env("OCI_SUBNET_OCID_MAD")
}

variable "oci_availability_domain_mad" {
  type    = string
  default = env("OCI_AVAILABILITY_DOMAIN_MAD")
}

# OCI per-region variables: eu-stockholm-1 (ARN)
variable "oci_subnet_ocid_arn" {
  type    = string
  default = env("OCI_SUBNET_OCID_ARN")
}

variable "oci_availability_domain_arn" {
  type    = string
  default = env("OCI_AVAILABILITY_DOMAIN_ARN")
}

# OCI per-region variables: eu-zurich-1 (ZRH)
variable "oci_subnet_ocid_zrh" {
  type    = string
  default = env("OCI_SUBNET_OCID_ZRH")
}

variable "oci_availability_domain_zrh" {
  type    = string
  default = env("OCI_AVAILABILITY_DOMAIN_ZRH")
}

# OCI per-region variables: eu-paris-1 (CDG)
variable "oci_subnet_ocid_cdg" {
  type    = string
  default = env("OCI_SUBNET_OCID_CDG")
}

variable "oci_availability_domain_cdg" {
  type    = string
  default = env("OCI_AVAILABILITY_DOMAIN_CDG")
}

# OCI per-region variables: uk-london-1 (LHR)
variable "oci_subnet_ocid_lhr" {
  type    = string
  default = env("OCI_SUBNET_OCID_LHR")
}

variable "oci_availability_domain_lhr" {
  type    = string
  default = env("OCI_AVAILABILITY_DOMAIN_LHR")
}

# OCI per-region variables: uk-cardiff-1 (CWL)
variable "oci_subnet_ocid_cwl" {
  type    = string
  default = env("OCI_SUBNET_OCID_CWL")
}

variable "oci_availability_domain_cwl" {
  type    = string
  default = env("OCI_AVAILABILITY_DOMAIN_CWL")
}

# ── Asia-Pacific (new) ────────────────────────────────────────────────

# OCI per-region variables: ap-seoul-1 (ICN)
variable "oci_subnet_ocid_icn" {
  type    = string
  default = env("OCI_SUBNET_OCID_ICN")
}

variable "oci_availability_domain_icn" {
  type    = string
  default = env("OCI_AVAILABILITY_DOMAIN_ICN")
}

# OCI per-region variables: ap-osaka-1 (KIX)
variable "oci_subnet_ocid_kix" {
  type    = string
  default = env("OCI_SUBNET_OCID_KIX")
}

variable "oci_availability_domain_kix" {
  type    = string
  default = env("OCI_AVAILABILITY_DOMAIN_KIX")
}

# OCI per-region variables: ap-melbourne-1 (MEL)
variable "oci_subnet_ocid_mel" {
  type    = string
  default = env("OCI_SUBNET_OCID_MEL")
}

variable "oci_availability_domain_mel" {
  type    = string
  default = env("OCI_AVAILABILITY_DOMAIN_MEL")
}

# OCI per-region variables: ap-sydney-1 (SYD)
variable "oci_subnet_ocid_syd" {
  type    = string
  default = env("OCI_SUBNET_OCID_SYD")
}

variable "oci_availability_domain_syd" {
  type    = string
  default = env("OCI_AVAILABILITY_DOMAIN_SYD")
}

# OCI per-region variables: ap-chuncheon-1 (YNJ)
variable "oci_subnet_ocid_ynj" {
  type    = string
  default = env("OCI_SUBNET_OCID_YNJ")
}

variable "oci_availability_domain_ynj" {
  type    = string
  default = env("OCI_AVAILABILITY_DOMAIN_YNJ")
}

# OCI per-region variables: ap-singapore-2 (XSP)
variable "oci_subnet_ocid_xsp" {
  type    = string
  default = env("OCI_SUBNET_OCID_XSP")
}

variable "oci_availability_domain_xsp" {
  type    = string
  default = env("OCI_AVAILABILITY_DOMAIN_XSP")
}

# ── Middle East / Africa (new) ────────────────────────────────────────

# OCI per-region variables: me-dubai-1 (DXB)
variable "oci_subnet_ocid_dxb" {
  type    = string
  default = env("OCI_SUBNET_OCID_DXB")
}

variable "oci_availability_domain_dxb" {
  type    = string
  default = env("OCI_AVAILABILITY_DOMAIN_DXB")
}

# OCI per-region variables: me-jeddah-1 (JED)
variable "oci_subnet_ocid_jed" {
  type    = string
  default = env("OCI_SUBNET_OCID_JED")
}

variable "oci_availability_domain_jed" {
  type    = string
  default = env("OCI_AVAILABILITY_DOMAIN_JED")
}

# OCI per-region variables: me-riyadh-1 (RUH)
variable "oci_subnet_ocid_ruh" {
  type    = string
  default = env("OCI_SUBNET_OCID_RUH")
}

variable "oci_availability_domain_ruh" {
  type    = string
  default = env("OCI_AVAILABILITY_DOMAIN_RUH")
}

# OCI per-region variables: il-jerusalem-1 (JRS)
variable "oci_subnet_ocid_jrs" {
  type    = string
  default = env("OCI_SUBNET_OCID_JRS")
}

variable "oci_availability_domain_jrs" {
  type    = string
  default = env("OCI_AVAILABILITY_DOMAIN_JRS")
}

# OCI per-region variables: af-johannesburg-1 (JNB)
variable "oci_subnet_ocid_jnb" {
  type    = string
  default = env("OCI_SUBNET_OCID_JNB")
}

variable "oci_availability_domain_jnb" {
  type    = string
  default = env("OCI_AVAILABILITY_DOMAIN_JNB")
}

# ── Latin America (new) ───────────────────────────────────────────────

# OCI per-region variables: sa-santiago-1 (SCL)
variable "oci_subnet_ocid_scl" {
  type    = string
  default = env("OCI_SUBNET_OCID_SCL")
}

variable "oci_availability_domain_scl" {
  type    = string
  default = env("OCI_AVAILABILITY_DOMAIN_SCL")
}

# OCI per-region variables: sa-bogota-1 (BOG)
variable "oci_subnet_ocid_bog" {
  type    = string
  default = env("OCI_SUBNET_OCID_BOG")
}

variable "oci_availability_domain_bog" {
  type    = string
  default = env("OCI_AVAILABILITY_DOMAIN_BOG")
}

# OCI per-region variables: sa-valparaiso-1 (VAP)
variable "oci_subnet_ocid_vap" {
  type    = string
  default = env("OCI_SUBNET_OCID_VAP")
}

variable "oci_availability_domain_vap" {
  type    = string
  default = env("OCI_AVAILABILITY_DOMAIN_VAP")
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

source "oracle-oci" "lantern-box-phx" {
  tenancy_ocid        = var.oci_tenancy_ocid
  user_ocid           = var.oci_user_ocid
  fingerprint         = var.oci_fingerprint
  key_file            = var.oci_key_file
  compartment_ocid    = var.oci_compartment_ocid
  availability_domain = var.oci_availability_domain_phx
  region              = "us-phoenix-1"

  base_image_filter {
    display_name_search = "^Canonical-Ubuntu-24.04-Minimal-aarch64-"
    operating_system    = "Canonical Ubuntu"
  }
  shape = "VM.Standard.A1.Flex"
  shape_config {
    ocpus         = 1
    memory_in_gbs = 1
  }
  subnet_ocid  = var.oci_subnet_ocid_phx
  ssh_username = "ubuntu"

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
    display_name_search = "^Canonical-Ubuntu-24.04-Minimal-aarch64-"
    operating_system    = "Canonical Ubuntu"
  }
  shape = "VM.Standard.A1.Flex"
  shape_config {
    ocpus         = 1
    memory_in_gbs = 1
  }
  subnet_ocid  = var.oci_subnet_ocid_ams
  ssh_username = "ubuntu"

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
    display_name_search = "^Canonical-Ubuntu-24.04-Minimal-aarch64-"
    operating_system    = "Canonical Ubuntu"
  }
  shape = "VM.Standard.A1.Flex"
  shape_config {
    ocpus         = 1
    memory_in_gbs = 1
  }
  subnet_ocid  = var.oci_subnet_ocid_bom
  ssh_username = "ubuntu"

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
    display_name_search = "^Canonical-Ubuntu-24.04-Minimal-aarch64-"
    operating_system    = "Canonical Ubuntu"
  }
  shape = "VM.Standard.A1.Flex"
  shape_config {
    ocpus         = 1
    memory_in_gbs = 1
  }
  subnet_ocid  = var.oci_subnet_ocid_gru
  ssh_username = "ubuntu"

  image_name = "lantern-box-${var.lantern_box_version}-arm64-gru"
}

# ── North America (new) ──────────────────────────────────────────────────

source "oracle-oci" "lantern-box-ord" {
  tenancy_ocid        = var.oci_tenancy_ocid
  user_ocid           = var.oci_user_ocid
  fingerprint         = var.oci_fingerprint
  key_file            = var.oci_key_file
  compartment_ocid    = var.oci_compartment_ocid
  availability_domain = var.oci_availability_domain_ord
  region              = "us-chicago-1"

  base_image_filter {
    display_name_search = "^Canonical-Ubuntu-24.04-Minimal-aarch64-"
    operating_system    = "Canonical Ubuntu"
  }
  shape = "VM.Standard.A1.Flex"
  shape_config {
    ocpus         = 1
    memory_in_gbs = 1
  }
  subnet_ocid  = var.oci_subnet_ocid_ord
  ssh_username = "ubuntu"

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
    display_name_search = "^Canonical-Ubuntu-24.04-Minimal-aarch64-"
    operating_system    = "Canonical Ubuntu"
  }
  shape = "VM.Standard.A1.Flex"
  shape_config {
    ocpus         = 1
    memory_in_gbs = 1
  }
  subnet_ocid  = var.oci_subnet_ocid_sjc
  ssh_username = "ubuntu"

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
    display_name_search = "^Canonical-Ubuntu-24.04-Minimal-aarch64-"
    operating_system    = "Canonical Ubuntu"
  }
  shape = "VM.Standard.A1.Flex"
  shape_config {
    ocpus         = 1
    memory_in_gbs = 1
  }
  subnet_ocid  = var.oci_subnet_ocid_yyz
  ssh_username = "ubuntu"

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
    display_name_search = "^Canonical-Ubuntu-24.04-Minimal-aarch64-"
    operating_system    = "Canonical Ubuntu"
  }
  shape = "VM.Standard.A1.Flex"
  shape_config {
    ocpus         = 1
    memory_in_gbs = 1
  }
  subnet_ocid  = var.oci_subnet_ocid_yul
  ssh_username = "ubuntu"

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
    display_name_search = "^Canonical-Ubuntu-24.04-Minimal-aarch64-"
    operating_system    = "Canonical Ubuntu"
  }
  shape = "VM.Standard.A1.Flex"
  shape_config {
    ocpus         = 1
    memory_in_gbs = 1
  }
  subnet_ocid  = var.oci_subnet_ocid_mty
  ssh_username = "ubuntu"

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
    display_name_search = "^Canonical-Ubuntu-24.04-Minimal-aarch64-"
    operating_system    = "Canonical Ubuntu"
  }
  shape = "VM.Standard.A1.Flex"
  shape_config {
    ocpus         = 1
    memory_in_gbs = 1
  }
  subnet_ocid  = var.oci_subnet_ocid_qro
  ssh_username = "ubuntu"

  image_name = "lantern-box-${var.lantern_box_version}-arm64-qro"
}

# ── Europe (new) ─────────────────────────────────────────────────────────

source "oracle-oci" "lantern-box-mrs" {
  tenancy_ocid        = var.oci_tenancy_ocid
  user_ocid           = var.oci_user_ocid
  fingerprint         = var.oci_fingerprint
  key_file            = var.oci_key_file
  compartment_ocid    = var.oci_compartment_ocid
  availability_domain = var.oci_availability_domain_mrs
  region              = "eu-marseille-1"

  base_image_filter {
    display_name_search = "^Canonical-Ubuntu-24.04-Minimal-aarch64-"
    operating_system    = "Canonical Ubuntu"
  }
  shape = "VM.Standard.A1.Flex"
  shape_config {
    ocpus         = 1
    memory_in_gbs = 1
  }
  subnet_ocid  = var.oci_subnet_ocid_mrs
  ssh_username = "ubuntu"

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
    display_name_search = "^Canonical-Ubuntu-24.04-Minimal-aarch64-"
    operating_system    = "Canonical Ubuntu"
  }
  shape = "VM.Standard.A1.Flex"
  shape_config {
    ocpus         = 1
    memory_in_gbs = 1
  }
  subnet_ocid  = var.oci_subnet_ocid_lin
  ssh_username = "ubuntu"

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
    display_name_search = "^Canonical-Ubuntu-24.04-Minimal-aarch64-"
    operating_system    = "Canonical Ubuntu"
  }
  shape = "VM.Standard.A1.Flex"
  shape_config {
    ocpus         = 1
    memory_in_gbs = 1
  }
  subnet_ocid  = var.oci_subnet_ocid_mad
  ssh_username = "ubuntu"

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
    display_name_search = "^Canonical-Ubuntu-24.04-Minimal-aarch64-"
    operating_system    = "Canonical Ubuntu"
  }
  shape = "VM.Standard.A1.Flex"
  shape_config {
    ocpus         = 1
    memory_in_gbs = 1
  }
  subnet_ocid  = var.oci_subnet_ocid_arn
  ssh_username = "ubuntu"

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
    display_name_search = "^Canonical-Ubuntu-24.04-Minimal-aarch64-"
    operating_system    = "Canonical Ubuntu"
  }
  shape = "VM.Standard.A1.Flex"
  shape_config {
    ocpus         = 1
    memory_in_gbs = 1
  }
  subnet_ocid  = var.oci_subnet_ocid_zrh
  ssh_username = "ubuntu"

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
    display_name_search = "^Canonical-Ubuntu-24.04-Minimal-aarch64-"
    operating_system    = "Canonical Ubuntu"
  }
  shape = "VM.Standard.A1.Flex"
  shape_config {
    ocpus         = 1
    memory_in_gbs = 1
  }
  subnet_ocid  = var.oci_subnet_ocid_cdg
  ssh_username = "ubuntu"

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
    display_name_search = "^Canonical-Ubuntu-24.04-Minimal-aarch64-"
    operating_system    = "Canonical Ubuntu"
  }
  shape = "VM.Standard.A1.Flex"
  shape_config {
    ocpus         = 1
    memory_in_gbs = 1
  }
  subnet_ocid  = var.oci_subnet_ocid_lhr
  ssh_username = "ubuntu"

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
    display_name_search = "^Canonical-Ubuntu-24.04-Minimal-aarch64-"
    operating_system    = "Canonical Ubuntu"
  }
  shape = "VM.Standard.A1.Flex"
  shape_config {
    ocpus         = 1
    memory_in_gbs = 1
  }
  subnet_ocid  = var.oci_subnet_ocid_cwl
  ssh_username = "ubuntu"

  image_name = "lantern-box-${var.lantern_box_version}-arm64-cwl"
}

# ── Asia-Pacific (new) ──────────────────────────────────────────────────

source "oracle-oci" "lantern-box-icn" {
  tenancy_ocid        = var.oci_tenancy_ocid
  user_ocid           = var.oci_user_ocid
  fingerprint         = var.oci_fingerprint
  key_file            = var.oci_key_file
  compartment_ocid    = var.oci_compartment_ocid
  availability_domain = var.oci_availability_domain_icn
  region              = "ap-seoul-1"

  base_image_filter {
    display_name_search = "^Canonical-Ubuntu-24.04-Minimal-aarch64-"
    operating_system    = "Canonical Ubuntu"
  }
  shape = "VM.Standard.A1.Flex"
  shape_config {
    ocpus         = 1
    memory_in_gbs = 1
  }
  subnet_ocid  = var.oci_subnet_ocid_icn
  ssh_username = "ubuntu"

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
    display_name_search = "^Canonical-Ubuntu-24.04-Minimal-aarch64-"
    operating_system    = "Canonical Ubuntu"
  }
  shape = "VM.Standard.A1.Flex"
  shape_config {
    ocpus         = 1
    memory_in_gbs = 1
  }
  subnet_ocid  = var.oci_subnet_ocid_kix
  ssh_username = "ubuntu"

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
    display_name_search = "^Canonical-Ubuntu-24.04-Minimal-aarch64-"
    operating_system    = "Canonical Ubuntu"
  }
  shape = "VM.Standard.A1.Flex"
  shape_config {
    ocpus         = 1
    memory_in_gbs = 1
  }
  subnet_ocid  = var.oci_subnet_ocid_mel
  ssh_username = "ubuntu"

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
    display_name_search = "^Canonical-Ubuntu-24.04-Minimal-aarch64-"
    operating_system    = "Canonical Ubuntu"
  }
  shape = "VM.Standard.A1.Flex"
  shape_config {
    ocpus         = 1
    memory_in_gbs = 1
  }
  subnet_ocid  = var.oci_subnet_ocid_syd
  ssh_username = "ubuntu"

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
    display_name_search = "^Canonical-Ubuntu-24.04-Minimal-aarch64-"
    operating_system    = "Canonical Ubuntu"
  }
  shape = "VM.Standard.A1.Flex"
  shape_config {
    ocpus         = 1
    memory_in_gbs = 1
  }
  subnet_ocid  = var.oci_subnet_ocid_ynj
  ssh_username = "ubuntu"

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
    display_name_search = "^Canonical-Ubuntu-24.04-Minimal-aarch64-"
    operating_system    = "Canonical Ubuntu"
  }
  shape = "VM.Standard.A1.Flex"
  shape_config {
    ocpus         = 1
    memory_in_gbs = 1
  }
  subnet_ocid  = var.oci_subnet_ocid_xsp
  ssh_username = "ubuntu"

  image_name = "lantern-box-${var.lantern_box_version}-arm64-xsp"
}

# ── Middle East / Africa (new) ──────────────────────────────────────────

source "oracle-oci" "lantern-box-dxb" {
  tenancy_ocid        = var.oci_tenancy_ocid
  user_ocid           = var.oci_user_ocid
  fingerprint         = var.oci_fingerprint
  key_file            = var.oci_key_file
  compartment_ocid    = var.oci_compartment_ocid
  availability_domain = var.oci_availability_domain_dxb
  region              = "me-dubai-1"

  base_image_filter {
    display_name_search = "^Canonical-Ubuntu-24.04-Minimal-aarch64-"
    operating_system    = "Canonical Ubuntu"
  }
  shape = "VM.Standard.A1.Flex"
  shape_config {
    ocpus         = 1
    memory_in_gbs = 1
  }
  subnet_ocid  = var.oci_subnet_ocid_dxb
  ssh_username = "ubuntu"

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
    display_name_search = "^Canonical-Ubuntu-24.04-Minimal-aarch64-"
    operating_system    = "Canonical Ubuntu"
  }
  shape = "VM.Standard.A1.Flex"
  shape_config {
    ocpus         = 1
    memory_in_gbs = 1
  }
  subnet_ocid  = var.oci_subnet_ocid_jed
  ssh_username = "ubuntu"

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
    display_name_search = "^Canonical-Ubuntu-24.04-Minimal-aarch64-"
    operating_system    = "Canonical Ubuntu"
  }
  shape = "VM.Standard.A1.Flex"
  shape_config {
    ocpus         = 1
    memory_in_gbs = 1
  }
  subnet_ocid  = var.oci_subnet_ocid_ruh
  ssh_username = "ubuntu"

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
    display_name_search = "^Canonical-Ubuntu-24.04-Minimal-aarch64-"
    operating_system    = "Canonical Ubuntu"
  }
  shape = "VM.Standard.A1.Flex"
  shape_config {
    ocpus         = 1
    memory_in_gbs = 1
  }
  subnet_ocid  = var.oci_subnet_ocid_jrs
  ssh_username = "ubuntu"

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
    display_name_search = "^Canonical-Ubuntu-24.04-Minimal-aarch64-"
    operating_system    = "Canonical Ubuntu"
  }
  shape = "VM.Standard.A1.Flex"
  shape_config {
    ocpus         = 1
    memory_in_gbs = 1
  }
  subnet_ocid  = var.oci_subnet_ocid_jnb
  ssh_username = "ubuntu"

  image_name = "lantern-box-${var.lantern_box_version}-arm64-jnb"
}

# ── Latin America (new) ─────────────────────────────────────────────────

source "oracle-oci" "lantern-box-scl" {
  tenancy_ocid        = var.oci_tenancy_ocid
  user_ocid           = var.oci_user_ocid
  fingerprint         = var.oci_fingerprint
  key_file            = var.oci_key_file
  compartment_ocid    = var.oci_compartment_ocid
  availability_domain = var.oci_availability_domain_scl
  region              = "sa-santiago-1"

  base_image_filter {
    display_name_search = "^Canonical-Ubuntu-24.04-Minimal-aarch64-"
    operating_system    = "Canonical Ubuntu"
  }
  shape = "VM.Standard.A1.Flex"
  shape_config {
    ocpus         = 1
    memory_in_gbs = 1
  }
  subnet_ocid  = var.oci_subnet_ocid_scl
  ssh_username = "ubuntu"

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
    display_name_search = "^Canonical-Ubuntu-24.04-Minimal-aarch64-"
    operating_system    = "Canonical Ubuntu"
  }
  shape = "VM.Standard.A1.Flex"
  shape_config {
    ocpus         = 1
    memory_in_gbs = 1
  }
  subnet_ocid  = var.oci_subnet_ocid_bog
  ssh_username = "ubuntu"

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
    display_name_search = "^Canonical-Ubuntu-24.04-Minimal-aarch64-"
    operating_system    = "Canonical Ubuntu"
  }
  shape = "VM.Standard.A1.Flex"
  shape_config {
    ocpus         = 1
    memory_in_gbs = 1
  }
  subnet_ocid  = var.oci_subnet_ocid_vap
  ssh_username = "ubuntu"

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
