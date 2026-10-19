variable "name" {
  description = "Name prefix for the backup bucket, e.g. \"ration-dev\"."
  type        = string
}

variable "noncurrent_version_expiration_days" {
  description = "Days to retain a snapshot's previous versions before S3 expires them (object versioning protects against a bad/corrupt snapshot overwrite)."
  type        = number
  default     = 90
}

variable "transition_to_glacier_days" {
  description = "Days after upload before a snapshot transitions to S3 Glacier Instant Retrieval (snapshots are cold data accessed only on disaster recovery)."
  type        = number
  default     = 30
}

variable "tags" {
  description = "Tags applied to every resource this module creates."
  type        = map(string)
  default     = {}
}
