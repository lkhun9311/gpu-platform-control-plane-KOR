terraform {
  # 1.10 is the floor because that is where the S3 backend learned to lock on a state-adjacent object
  # (use_lockfile) instead of a DynamoDB table. Everything below that line needs the table back.
  required_version = ">= 1.10"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.60"
    }
    tls = {
      source  = "hashicorp/tls"
      version = "~> 4.0"
    }
  }
}

provider "aws" {
  region = var.region
}
