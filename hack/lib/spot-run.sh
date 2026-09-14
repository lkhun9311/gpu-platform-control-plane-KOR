#!/usr/bin/env bash
#
# EC2 Spot lifecycle for a single-instance measurement run.
#
# WHY THIS IS A LIBRARY
#
# hack/m5b-scheduler-microtest.sh uses this today and the price-of-protection runner will need the same
# scaffolding: a results bucket that expires, an instance profile, a GPU AMI, a default public subnet in a
# zone that actually offers the instance type, a Spot launch that survives "no capacity right now",
# completion polling, and teardown. That is about 150 lines, and the alternative to sharing them is a
# second copy written from memory of the first.
#
# This repository has already paid twice for a hand-kept list drifting into copies: one paid run answered
# a quarter of every replay with 401, the next answered its probe tenants with 403. Cost controls, IAM and
# teardown are exactly the invariants that should have one implementation.
#
# WHAT IS NOT HERE
#
# The experiment. User-data construction, what the instance does, which artifacts count as a complete
# result, and the run directory's layout all belong to the caller. A library that reached into those would
# be a second runner wearing a library's name.
#
# RULES THIS FILE FOLLOWS, AND WHY
#
#   * It never calls `set`. Shell options belong to the entrypoint: hack/m5b-scheduler-microtest.sh runs
#     under `set -euo pipefail` and hack/m5b-arms.sh deliberately does not, checking failures explicitly
#     instead. A library that set either would silently change the other's error handling.
#   * It never runs `cd`, and it uses ${BASH_SOURCE[0]} only to locate itself. Inside a sourced file $0 is
#     still the entrypoint, so `cd "$(dirname "$0")/.."` here would move to a valid but wrong directory
#     without saying so.
#   * It installs no traps. Traps do not stack -- the last one for a signal replaces the earlier one -- so a
#     library trap would silently discard the caller's cleanup or be discarded by it. Terminating an
#     instance is offered as a function and armed by whoever owns the instance id.
#   * Every function takes explicit arguments and declares its temporaries `local`. The version of this code
#     that lived inline had a launch helper capturing a mutable global SUBNET, which is a data dependency
#     nothing declares.
#   * No parameter has a default. A default that every caller overrides is dead code that looks like a
#     safety net: changing the library's 20-second IAM propagation wait or its 30-second poll interval
#     moved nothing, and the characterization harness correctly reported no change, which is the worst way
#     to learn a value is not the one in use. Every number is at a call site where it can be read.
#   * stdout carries return data only. Diagnostics go to stderr, because a stray echo inside a function
#     whose output is captured corrupts the value while still passing a non-empty check.
#
# Callers are expected to provide `spot_say` and `spot_fail` or accept the defaults below.

# Defaults, defined only if the caller has not. A caller with its own say/fail keeps them.
if ! declare -F spot_say >/dev/null 2>&1; then
  spot_say() { printf '== %s\n' "$*" >&2; }
fi
if ! declare -F spot_fail >/dev/null 2>&1; then
  spot_fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
fi

# spot_account echoes the account id of the current credentials.
#
# No --region: the call is global, and adding one would be a change to what the runner does dressed up as
# a refactor. The characterization goldens record the exact argv, which is how that would have been caught.
spot_account() {
  aws sts get-caller-identity --query Account --output text
}

# spot_ensure_bucket creates a private, expiring results bucket if it does not exist.
#
# Expiring on purpose: the results are a few kilobytes and interesting for about a week, nothing here is
# worth paying storage for indefinitely, and a bucket that expires cannot become the next thing nobody
# remembers deleting.
spot_ensure_bucket() {
  local bucket="$1" region="$2" expire_days="$3"
  if aws s3api head-bucket --bucket "$bucket" 2>/dev/null; then
    return 0
  fi
  spot_say "creating results bucket $bucket"
  aws s3api create-bucket --bucket "$bucket" --region "$region" \
    --create-bucket-configuration LocationConstraint="$region" >/dev/null || return 1
  aws s3api put-public-access-block --bucket "$bucket" \
    --public-access-block-configuration BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true || return 1
  aws s3api put-bucket-lifecycle-configuration --bucket "$bucket" --lifecycle-configuration \
    "{\"Rules\":[{\"ID\":\"expire\",\"Status\":\"Enabled\",\"Filter\":{\"Prefix\":\"\"},\"Expiration\":{\"Days\":$expire_days}}]}"
}

# spot_ensure_profile creates an EC2 instance profile carrying the caller's S3 policy.
#
# The policy is the CALLER'S, because authorization is not shared: the microtest only writes results, while
# a runner that ships a binary to the instance also has to read one back. Passing it in keeps the library
# from having to know which.
spot_ensure_profile() {
  local name="$1" policy="$2" propagation_wait="$3"
  if aws iam get-instance-profile --instance-profile-name "$name" >/dev/null 2>&1; then
    return 0
  fi
  spot_say "creating instance role $name"
  aws iam create-role --role-name "$name" --assume-role-policy-document \
    '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole"}]}' >/dev/null || return 1
  aws iam put-role-policy --role-name "$name" --policy-name write-results --policy-document "$policy" || return 1
  aws iam create-instance-profile --instance-profile-name "$name" >/dev/null || return 1
  aws iam add-role-to-instance-profile --instance-profile-name "$name" --role-name "$name" || return 1
  # An instance profile is not usable the moment it is created, and a launch that races it fails with an
  # error naming neither the profile nor the delay.
  spot_say "waiting for the instance profile to propagate"
  sleep "$propagation_wait"
}

# spot_resolve_ami echoes the AMI id an SSM parameter currently points at.
spot_resolve_ami() {
  local region="$1" parameter="$2"
  aws ssm get-parameter --region "$region" --name "$parameter" --query Parameter.Value --output text
}

# spot_zones_offering echoes the availability zones that actually offer an instance type.
#
# Not every zone in a region does, and the first version of this code took Subnets[0], landed in
# ap-northeast-2b, and was refused because g5 is not offered there. AWS will say which zones do, so it is
# asked rather than assumed.
spot_zones_offering() {
  local region="$1" instance_type="$2"
  aws ec2 describe-instance-type-offerings --region "$region" \
    --location-type availability-zone \
    --filters "Name=instance-type,Values=$instance_type" \
    --query 'InstanceTypeOfferings[].Location' --output text
}

# spot_subnet_in_zone echoes the default public subnet of a zone, or nothing.
#
# A DEFAULT subnet, because default subnets assign public IPs and therefore reach the internet through an
# internet gateway rather than a NAT gateway. That is the whole cost argument: inbound is free there, and
# the same 15.6 GB of image and weights crossed a NAT gateway at $0.059/GB in every earlier paid run.
spot_subnet_in_zone() {
  local region="$1" zone="$2" subnet
  subnet=$(aws ec2 describe-subnets --region "$region" \
    --filters Name=default-for-az,Values=true Name=map-public-ip-on-launch,Values=true \
      "Name=availability-zone,Values=$zone" \
    --query 'Subnets[0].SubnetId' --output text 2>/dev/null)
  [ -n "$subnet" ] && [ "$subnet" != "None" ] || return 1
  printf '%s' "$subnet"
}

# spot_launch starts one Spot instance and echoes its id.
#
# Every parameter is explicit rather than read from the caller's environment, because the version of this
# that lived inline captured a mutable SUBNET from its enclosing scope and could therefore launch into a
# zone other than the one its log line named.
spot_launch() {
  local region="$1" ami="$2" instance_type="$3" subnet="$4" profile="$5" \
        max_price="$6" volume_gb="$7" user_data_file="$8" tags="$9"
  aws ec2 run-instances --region "$region" \
    --image-id "$ami" --instance-type "$instance_type" --subnet-id "$subnet" \
    --iam-instance-profile "Name=$profile" \
    --instance-initiated-shutdown-behavior terminate \
    --block-device-mappings "[{\"DeviceName\":\"/dev/sda1\",\"Ebs\":{\"VolumeSize\":$volume_gb,\"VolumeType\":\"gp3\",\"DeleteOnTermination\":true}}]" \
    --instance-market-options "{\"MarketType\":\"spot\",\"SpotOptions\":{\"MaxPrice\":\"$max_price\",\"SpotInstanceType\":\"one-time\"}}" \
    --user-data "file://$user_data_file" \
    --tag-specifications "$tags" \
    --query 'Instances[0].InstanceId' --output text
}

# spot_terminate ends an instance, and SHOUTS if it could not.
#
# Offered rather than trapped: traps do not stack, so whoever owns the instance id arms this.
#
# It used to end in `>/dev/null 2>&1 || true`, which discarded the reason and swallowed the failure, so a
# termination that did not happen printed "terminating i-..." and returned success. On 2026-09-08 that is
# exactly what it did: credentials expired mid-run, the trap fired, the API call was refused, and the line
# on screen said the instance was being terminated while it went on billing until someone noticed and killed
# it by hand. A cleanup path that cannot tell "did not run" from "passed" is the failure class this
# repository has been bitten by most, and this is the one place where it costs money.
#
# It still returns 0. The caller arms this from an EXIT trap, and a trap that fails would overwrite the
# run's own exit status with the cleanup's -- which is how a successful run would start reporting failure.
# The report is the message, not the status.
spot_terminate() {
  local region="$1" instance_id="$2" err state
  [ -n "$instance_id" ] || return 0
  spot_say "terminating $instance_id"
  if ! err=$(aws ec2 terminate-instances --region "$region" --instance-ids "$instance_id" 2>&1 >/dev/null); then
    printf 'TERMINATE FAILED for %s: %s\n' "$instance_id" "$err" >&2
    # An instance that is already gone is not a problem, so ask before raising the alarm. The state call
    # needs credentials too, and "unknown" means it could not be asked -- which is not the same as running,
    # and both are worth saying out loud.
    state=$(spot_instance_state "$region" "$instance_id")
    case "$state" in
      terminated|shutting-down)
        printf 'it is %s anyway, so nothing is still billing\n' "$state" >&2 ;;
      *)
        printf 'STILL %s AND BILLING. Terminate it by hand:\n  aws ec2 terminate-instances --region %s --instance-ids %s\n' \
          "$state" "$region" "$instance_id" >&2 ;;
    esac
  fi
  return 0
}

# spot_instance_state echoes an instance's state, or "unknown".
spot_instance_state() {
  local region="$1" instance_id="$2"
  aws ec2 describe-instances --region "$region" --instance-ids "$instance_id" \
    --query 'Reservations[0].Instances[0].State.Name' --output text 2>/dev/null || echo unknown
}

# spot_wait_for_marker polls for a completion key and reports WHY it stopped.
#
# Three endings, three exit statuses, because they need different answers and used to arrive as one
# message. 0 means the marker appeared. 2 means the instance ended before writing one, and its log is worth
# reading. 3 means the poll budget ran out, and the run is probably still going.
#
# The marker key is the caller's, and it should be scoped to the run: a bucket that keeps objects for
# thirty days plus a fixed key at its root means the next run finds the previous run's marker on its
# opening poll, downloads the previous run's results and reports them as its own.
# On exit status 2 the instance's terminal state is echoed, because "terminated" and "shutting-down" are
# different things to read in a log and the caller's refusal quotes it.
#
# Nothing here calls spot_say. A caller may point spot_say at its own stdout logger, and this function's
# stdout is captured by the caller, so a diagnostic written there would be read back as the state.
spot_wait_for_marker() {
  local region="$1" bucket="$2" key="$3" instance_id="$4" attempts="$5" interval="$6"
  local n state
  for n in $(seq 1 "$attempts"); do
    if aws s3api head-object --bucket "$bucket" --key "$key" >/dev/null 2>&1; then
      return 0
    fi
    state=$(spot_instance_state "$region" "$instance_id")
    case "$state" in
      terminated|shutting-down) printf '%s' "$state"; return 2 ;;
    esac
    sleep "$interval"
  done
  return 3
}
