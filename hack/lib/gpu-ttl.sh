#!/usr/bin/env bash
# A deadline that does not run on this laptop.
#
# Every other teardown in this repository is a trap in the operator's shell, and a trap does not run after
# the machine sleeps, loses power, or has its shell SIGKILLed. It also does not run usefully once the SSO
# session expires, because the scale-down is itself an AWS call: the credential that stops the billing dies
# at the same moment the session does.
#
# So the deadline is registered with AWS before the GPU starts, and AWS enforces it whatever happens here.
# EventBridge Scheduler calls eks:UpdateNodegroupConfig through a universal target -- no Lambda, nothing to
# package, and no second copy of the teardown logic to drift from the first.
#
# The order is the point: arm, verify, THEN scale up. A session that cannot register its deadline does not
# get a GPU, because the alternative is a GPU with no deadline.

TTL_SCHEDULE_NAME=""

# ttl_arm CLUSTER NODEGROUP MINUTES
#
# Registers a one-shot schedule that forces the node group to desiredSize=0 after MINUTES.
# Returns non-zero if the schedule could not be created, and the caller must treat that as fatal.
ttl_arm() {
  local cluster="$1" nodegroup="$2" minutes="$3"
  local role name at

  # Two ways to learn the role, because the first one has a failure mode that stops a paid session for a
  # reason that has nothing to do with the session.
  #
  # The callers read TTL_ROLE_ARN from `terraform output`, which needs a working backend and live
  # credentials for the state bucket's KMS key. It fails with InvalidGrantException on an expired SSO token
  # -- observed on 2026-08-31 while the role itself was perfectly readable through the IAM API a second
  # later. Refusing to start was the safe direction, but refusing because a state file could not be opened,
  # when the thing being looked up is a role name that has not changed, is a bad trade.
  #
  # IAM is the fallback and the name is a constant here for the same reason the node group names are: it is
  # set in infra/aws/bootstrap/ttl.tf and does not vary per session.
  role="${TTL_ROLE_ARN:-}"
  if [ -z "$role" ]; then
    role=$(aws iam get-role --role-name "${TTL_ROLE_NAME:-gpu-platform-ttl-scaledown}" \
             --query 'Role.Arn' --output text 2>/dev/null)
  fi
  if [ -z "$role" ] || [ "$role" = "None" ]; then
    echo "could not determine the TTL scale-down role. Apply infra/aws/bootstrap, or pass TTL_ROLE_ARN." >&2
    return 1
  fi

  # A second schedule on the same node group is not a second layer of safety; it is a hidden earlier
  # deadline. Names carry a timestamp so they never collide, which meant nothing stopped a session from
  # arming on top of a leftover schedule from an interrupted run -- and the leftover, being older, fires
  # first and scales the card out from under the new session. ttl_remaining_minutes would have been reading
  # whichever one the API happened to return first, so the length guards would have been measuring the wrong
  # clock as well.
  #
  # Refusing is the same answer used for a second running card, for the same reason: this process cannot
  # tell an abandoned schedule from one another session is relying on. TTL_REPLACE=1 is the deliberate
  # override for resuming a session whose own schedule is known to be stale.
  local existing
  existing=$(aws scheduler list-schedules --name-prefix "gpu-ttl-${cluster}-${nodegroup}-" \
               --query 'Schedules[].Name' --output text 2>/dev/null)
  if [ -n "$existing" ] && [ "$existing" != "None" ]; then
    if [ "${TTL_REPLACE:-}" = "1" ]; then
      for old_name in $existing; do
        aws scheduler delete-schedule --name "$old_name" >/dev/null 2>&1 \
          && echo "TTL_REPLACE=1: deleted the earlier schedule $old_name" >&2
      done
    else
      echo "a deadline is already registered for $cluster/$nodegroup: $existing" >&2
      echo "  An older schedule fires first and would cut this session short. If it is left over from an" >&2
      echo "  interrupted run, re-run with TTL_REPLACE=1. If another session is using it, wait for that one." >&2
      return 1
    fi
  fi

  # UTC, because Scheduler's at() expressions are not timezone-aware unless one is passed, and a session that
  # armed its deadline nine hours late would be worse than one that never armed it at all.
  at=$(date -u -d "+${minutes} minutes" '+%Y-%m-%dT%H:%M:%S')
  name="gpu-ttl-${cluster}-${nodegroup}-$(date -u '+%Y%m%d%H%M%S')"

  # ActionAfterCompletion=DELETE so a finished deadline does not accumulate. A session that ends cleanly
  # deletes its own schedule; this covers the one that does not.
  if ! aws scheduler create-schedule \
        --name "$name" \
        --schedule-expression "at($at)" \
        --schedule-expression-timezone UTC \
        --flexible-time-window '{"Mode":"OFF"}' \
        --action-after-completion DELETE \
        --description "Force $cluster/$nodegroup to desiredSize=0 if the session that armed this never finished." \
        --target "{
            \"Arn\": \"arn:aws:scheduler:::aws-sdk:eks:updateNodegroupConfig\",
            \"RoleArn\": \"$role\",
            \"Input\": \"{\\\"ClusterName\\\":\\\"$cluster\\\",\\\"NodegroupName\\\":\\\"$nodegroup\\\",\\\"ScalingConfig\\\":{\\\"MinSize\\\":0,\\\"DesiredSize\\\":0}}\"
          }" >/dev/null; then
    echo "could not register the TTL scale-down for $cluster/$nodegroup" >&2
    return 1
  fi

  # Claim the name BEFORE verifying it, so a schedule that exists is never one this process has forgotten
  # about. The read-back below can fail on a transient API error after a create that actually succeeded; the
  # old order returned failure with TTL_SCHEDULE_NAME still empty, which left a real schedule that no
  # cleanup could delete and that would then cut the NEXT session on this node group short.
  TTL_SCHEDULE_NAME="$name"

  # Read it back. An accepted create that produced no schedule is the failure this whole file exists to make
  # impossible, and it is silent unless something looks.
  if ! aws scheduler get-schedule --name "$name" >/dev/null 2>&1; then
    echo "the TTL schedule $name was accepted but cannot be read back; deleting it so it cannot fire on a" >&2
    echo "  session that never learned it existed" >&2
    aws scheduler delete-schedule --name "$name" >/dev/null 2>&1 \
      || echo "  WARNING: could not delete $name either. Delete it by hand before the next session." >&2
    TTL_SCHEDULE_NAME=""
    return 1
  fi

  TTL_SCHEDULE_NAME="$name"
  echo "TTL armed: $cluster/$nodegroup goes to desiredSize=0 at ${at}Z (in ${minutes}m), schedule $name" >&2
  return 0
}

# ttl_disarm
#
# Deletes the schedule this session armed. Safe to call when nothing was armed.
#
# Failure here is reported and not fatal: the schedule firing later is harmless -- it sets a node group that
# is already at zero to zero -- whereas failing the session over a leftover deadline would be the tail
# wagging the dog.
# ttl_remaining_minutes CLUSTER NODEGROUP
#
# Prints how many minutes are left on the live deadline for that node group, or nothing if there is none.
#
# This exists because the script that spends the money is not always the script that armed the deadline:
# hack/m5b-arms.sh runs in a second shell, with no TTL_SCHEDULE_NAME and no memory of TTL_MINUTES, and it is
# the one that decides whether a run fits. Asking AWS is also the only honest answer -- the deadline may have
# been armed an hour ago, and what matters is the time that is left, not the time it started with.
ttl_remaining_minutes() {
  local cluster="$1" nodegroup="$2" expr at now
  # Every schedule on this target, not Schedules[0]. The API gives no ordering guarantee, and the answer
  # that matters is the EARLIEST deadline -- that is the one that will actually cut the session. Reading an
  # arbitrary one meant the length guards could be measuring a clock that fires an hour after the one that
  # will really stop the card.
  local names n at_n earliest=""
  names=$(aws scheduler list-schedules --name-prefix "gpu-ttl-${cluster}-${nodegroup}-" \
            --query 'Schedules[].Name' --output text 2>/dev/null)
  [ -n "$names" ] && [ "$names" != "None" ] || return 1
  for n in $names; do
    at_n=$(aws scheduler get-schedule --name "$n" --query 'ScheduleExpression' --output text 2>/dev/null \
             | sed -n 's/^at(\(.*\))$/\1/p')
    [ -n "$at_n" ] || continue
    if [ -z "$earliest" ] || [ "$at_n" \< "$earliest" ]; then earliest="$at_n"; fi
  done
  [ -n "$earliest" ] || return 1
  expr="$earliest"
  at="$expr"
  now=$(date -u '+%s')
  at=$(date -u -d "${at}Z" '+%s' 2>/dev/null) || return 1
  echo $(( (at - now) / 60 ))
}

ttl_disarm() {
  [ -n "$TTL_SCHEDULE_NAME" ] || return 0
  if aws scheduler delete-schedule --name "$TTL_SCHEDULE_NAME" >/dev/null 2>&1; then
    echo "TTL disarmed: $TTL_SCHEDULE_NAME" >&2
    TTL_SCHEDULE_NAME=""
  else
    # The name is deliberately kept. Clearing it unconditionally meant a failed delete looked identical to a
    # successful one: the EXIT trap's later cleanup had nothing left to retry with, and the schedule stayed.
    # Harmless while it targets an already-zero group, but the next session on this node group inherits it
    # as an earlier deadline -- which is exactly the state ttl_arm now refuses to start on.
    echo "WARNING: could not delete the TTL schedule $TTL_SCHEDULE_NAME. It will fire against an" >&2
    echo "  already-zero node group, which is harmless now, but the next session here will refuse to start" >&2
    echo "  until it is gone: aws scheduler delete-schedule --name $TTL_SCHEDULE_NAME" >&2
    return 1
  fi
}
