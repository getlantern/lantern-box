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

# ---------- North America (new) ----------------------------------------

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

# ---------- Europe (new) -----------------------------------------------

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

# ---------- Asia-Pacific (new) -----------------------------------------

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

# ---------- Middle East / Africa (new) ----------------------------------

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

# ---------- Latin America (new) -----------------------------------------

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
