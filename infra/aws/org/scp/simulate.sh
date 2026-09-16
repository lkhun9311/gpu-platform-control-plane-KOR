#!/usr/bin/env bash
# Dry-run the SCPs against what this account has actually called.
#
# An SCP cannot be tested by attaching it and watching: the first thing you learn is that teardown is denied
# while a GPU bills. So the check runs the other way -- take every call CloudTrail recorded, and ask whether
# any policy here would have refused it. A match is a deny that would have broken something that already ran.
#
# This proves the policies do not break the RECORDED paths. It cannot prove they do not break an unrecorded
# one, which is why README.md requires a full non-GPU apply/destroy cycle under the policy before the lab
# account is moved into the OU that carries it.
set -uo pipefail
cd "$(dirname "$0")"

DAYS="${DAYS:-7}"
REGIONS="${REGIONS:-ap-northeast-2 us-east-1}"
ALLOWED_REGIONS="ap-northeast-2 us-east-1"
ALLOWED_TYPES="t3. g4dn. g5."

say() { printf '\n== %s\n' "$*"; }
fail=0

# Ask AWS whether the policy is even well-formed, before asking what it would have done.
#
# This step exists because the simulation below cannot see the difference between a condition that never
# matches and a condition key that DOES NOT EXIST. It substitutes recorded calls into the policy and reports
# what would have been denied; a nonexistent key simply never matches, so the run comes back clean.
#
# The console caught what this script could not: eks:instanceTypes is not a condition key EKS publishes, so
# DenyUnapprovedInstanceTypesOnManagedNodeGroups could not be created at all -- Organizations refuses the
# policy. Three rounds of review on that one statement had already found a deny that could not fire and a
# checker that duplicated its action list, and neither round asked whether the key was real.
#
# IAM Access Analyzer answers exactly that question, with the same finding code the console shows
# (INVALID_SERVICE_CONDITION_KEY). Verified by putting the rejected statement back and watching this fail.
say "validating the policies against AWS's own grammar"
for f in deny-region.json deny-instance-family.json deny-escape.json; do
  findings=$(aws accessanalyzer validate-policy \
      --policy-document "file://$f" --policy-type SERVICE_CONTROL_POLICY \
      --query 'findings[?findingType==`ERROR` || findingType==`SECURITY_WARNING`].[findingType,issueCode]' \
      --output text 2>&1)
  if [ -n "$findings" ]; then
    echo "   $f"
    echo "$findings" | sed 's/^/     /'
    fail=1
  else
    echo "   $f: no errors"
  fi
done

say "collecting CloudTrail events (last ${DAYS} days)"
: > /tmp/scp-events.jsonl
for r in $REGIONS; do
  n=$(aws cloudtrail lookup-events --region "$r" \
        --start-time "$(date -u -d "-${DAYS} days" +%Y-%m-%dT%H:%M:%SZ)" \
        --max-results 200 --query 'Events[].CloudTrailEvent' --output text 2>/dev/null \
      | tr '\t' '\n' | grep -c . || true)
  aws cloudtrail lookup-events --region "$r" \
      --start-time "$(date -u -d "-${DAYS} days" +%Y-%m-%dT%H:%M:%SZ)" \
      --max-results 200 --query 'Events[].CloudTrailEvent' --output text 2>/dev/null \
    | tr '\t' '\n' | grep . >> /tmp/scp-events.jsonl || true
  echo "   $r: $n events"
done
total=$(wc -l < /tmp/scp-events.jsonl)
echo "   total: $total"
[ "$total" -eq 0 ] && { echo "   no events -- the simulation proves nothing. Widen DAYS or run a session first."; exit 1; }

say "deny-region: any recorded call outside the allowed Regions?"
python3 - "$ALLOWED_REGIONS" <<'PY'
import sys, json
allowed = set(sys.argv[1].split())
# The exemption list in deny-region.json, as service prefixes.
exempt = set(json.load(open('deny-region.json'))['Statement'][0]['NotAction'])
exempt_prefixes = {a.split(':')[0] for a in exempt}
bad = []
for line in open('/tmp/scp-events.jsonl'):
    try: e = json.loads(line)
    except Exception: continue
    region = e.get('awsRegion')
    svc = e.get('eventSource', '').split('.')[0]
    if region in allowed or svc in exempt_prefixes:
        continue
    bad.append((region, svc, e.get('eventName')))
if bad:
    print(f"   WOULD HAVE DENIED {len(bad)}:")
    for r, s, n in bad[:10]:
        print(f"     {r} {s}:{n}")
    sys.exit(1)
print("   clean -- every recorded call is in an allowed Region or an exempt service")
PY
[ $? -ne 0 ] && fail=1

say "deny-instance-family: any recorded launch of an unapproved type?"
python3 - "$ALLOWED_TYPES" <<'PY'
import sys, json
allowed = tuple(sys.argv[1].split())
# Read the watched actions out of the policy rather than restating them. A hardcoded copy drifts from the
# file it is supposed to be checking, which is how the first version of this script kept warning about
# UpdateNodegroupConfig after the policy had stopped denying it.
watched = set()
for s in json.load(open('deny-instance-family.json'))['Statement']:
    a = s['Action']
    watched.update(x.split(':')[1] for x in (a if isinstance(a, list) else [a]))
seen = bad = 0
for line in open('/tmp/scp-events.jsonl'):
    try: e = json.loads(line)
    except Exception: continue
    if e.get('eventName') not in watched:
        continue
    seen += 1
    blob = json.dumps(e.get('requestParameters') or {})
    types = [t for t in ('t3.', 'g4dn.', 'g5.', 'p3.', 'p4d.', 'p5.', 'inf', 'trn', 'm5.', 'c5.') if t in blob]
    unapproved = [t for t in types if not t.startswith(allowed)]
    if unapproved:
        bad += 1
        print(f"   WOULD HAVE DENIED {e.get('eventName')}: {unapproved}")
print(f"   {seen} launch-shaped calls recorded, {bad} would have been denied")
sys.exit(1 if bad else 0)
PY
[ $? -ne 0 ] && fail=1

say "deny-escape: any recorded call this policy forbids?"
python3 <<'PY'
import json, sys
denied = set()
for s in json.load(open('deny-escape.json'))['Statement']:
    a = s['Action']
    denied.update(a if isinstance(a, list) else [a])
names = {d.split(':')[1] for d in denied}
hits = []
for line in open('/tmp/scp-events.jsonl'):
    try: e = json.loads(line)
    except Exception: continue
    if e.get('eventName') in names:
        hits.append(e.get('eventName'))
if hits:
    print(f"   WOULD HAVE DENIED: {sorted(set(hits))}")
    sys.exit(1)
print("   clean -- nothing recorded calls these")
PY
[ $? -ne 0 ] && fail=1

say "teardown path: does any policy touch a cleanup call?"
python3 <<'PY'
# The calls destroy.yml and the session scripts make on the way down. A deny here is the failure that costs
# money rather than the one that saves it.
cleanup = ['TerminateInstances', 'DeleteNodegroup', 'UpdateNodegroupConfig', 'DeleteCluster',
           'DeleteNatGateway', 'ReleaseAddress', 'DeleteVpc', 'DeleteSubnet', 'DeleteSecurityGroup',
           'DetachInternetGateway', 'DeleteInternetGateway', 'DeleteRoute', 'DeleteRouteTable',
           'DeleteNetworkInterface', 'DeleteLoadBalancer', 'DeleteVolume', 'ScheduleKeyDeletion']
import json
region_exempt = {a.split(':')[0] for a in json.load(open('deny-region.json'))['Statement'][0]['NotAction']}
family_watched = set()
for s in json.load(open('deny-instance-family.json'))['Statement']:
    a = s['Action']
    family_watched.update(x.split(':')[1] for x in (a if isinstance(a, list) else [a]))
escape = set()
for s in json.load(open('deny-escape.json'))['Statement']:
    a = s['Action']
    escape.update(x.split(':')[1] for x in (a if isinstance(a, list) else [a]))
risk = []
for c in cleanup:
    if c in escape:
        risk.append((c, 'deny-escape'))
    elif c in family_watched:
        # Conditional: only denied when the instance type is unapproved. Scale-down to zero sends no type.
        risk.append((c, 'deny-instance-family (conditional on instanceTypes -- scale-down sends none)'))
for c, why in risk:
    print(f"   REVIEW {c}: {why}")
if not risk:
    print("   clean -- no cleanup call is denied unconditionally")
else:
    print("   the conditional ones are safe only if the call omits instanceTypes; verify in the dry run")
PY

say "result"
if [ "$fail" -ne 0 ]; then
  echo "   FAIL -- a policy would have denied something that already ran. Do not attach."
  exit 1
fi
echo "   PASS on recorded calls. This is necessary and not sufficient: run a full non-GPU"
echo "   apply/destroy under the policy before moving the lab account into its OU."
