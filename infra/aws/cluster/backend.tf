# The backend points at the bucket created by infra/aws/bootstrap.
#
# terraform validate runs with -backend=false, so this is inert until a real init.
terraform {
  backend "s3" {
    key     = "cluster/terraform.tfstate"
    region  = "ap-northeast-2"
    encrypt = true

    # State locking without a DynamoDB table.
    #
    # Terraform's S3 backend used to have no way to serialise concurrent applies -- S3 had no compare-and-set
    # -- so it borrowed a DynamoDB table with a LockID hash key purely as a mutex. S3 gained conditional
    # writes, the backend gained use_lockfile in 1.10, and the table became a second service to provision,
    # encrypt, back up, grant IAM on and pay for, in order to hold one row.
    #
    # The lock is now an object beside the state at <key>.tflock, written with If-None-Match.
    use_lockfile = true
  }
}
