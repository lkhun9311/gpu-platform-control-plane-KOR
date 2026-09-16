#!/usr/bin/env bash
# Checks that the rendered webhook overlay's cross-references actually resolve.
#
# Every one of these was wrong in the first version, and kustomize built it without complaint: the webhook
# pointed at a Service named "webhook-service" in a namespace called "system", and the CA annotation named a
# Certificate at "system/serving-cert". Nothing in the YAML was malformed. The names simply referred to objects
# that would never exist, and the only symptom in a cluster would have been every MLTrainingJob write refused
# by a failurePolicy of Fail.
#
# A schema check cannot catch that, because each document is individually valid. What has to be checked is
# whether the names MATCH each other.
set -euo pipefail

cd "$(dirname "$0")/.."
KUSTOMIZE=${KUSTOMIZE:-./bin/kustomize}
RENDERED=$("$KUSTOMIZE" build config/webhook-enabled)

python3 - "$RENDERED" <<'PY'
import re, sys

docs = sys.argv[1].split('\n---\n')
d = {}
for doc in docs:
    if 'kind: ValidatingWebhookConfiguration' in doc: d['wh'] = doc
    elif 'kind: Service' in doc and 'webhook-service' in doc: d['svc'] = doc
    elif 'kind: Certificate' in doc: d['cert'] = doc
    elif 'kind: Deployment' in doc: d['dep'] = doc

missing = [k for k in ('wh', 'svc', 'cert', 'dep') if k not in d]
if missing:
    sys.exit('overlay is missing documents: %s' % ', '.join(missing))

wh, svc, cert, dep = d['wh'], d['svc'], d['cert'], d['dep']

def one(pattern, text, what):
    m = re.search(pattern, text, re.M)
    if not m:
        sys.exit('could not read %s from the rendered overlay' % what)
    return m

svc_name = one(r'name: (\S*webhook-service)', svc, 'the Service name').group(1)
svc_ns = one(r'namespace: (\S+)', svc, 'the Service namespace').group(1)
target = one(r'name: (\S*webhook-service)\n\s*namespace: (\S+)', wh, "the webhook's service reference")
inject = one(r'inject-ca-from: (\S+)', wh, 'the CA injection annotation').group(1)
cert_name = one(r'name: (\S*serving-cert)', cert, 'the Certificate name').group(1)
cert_ns = one(r'namespace: (\S+)', cert, 'the Certificate namespace').group(1)
cert_secret = one(r'secretName: (\S+)', cert, "the Certificate's secretName").group(1)
dep_secret = one(r'secretName: (\S+)', dep, "the Deployment's mounted secret").group(1)
cert_dns = re.findall(r'- (\S+\.svc)$', cert, re.M)

failures = []
def require(cond, msg):
    if not cond:
        failures.append(msg)

require(target.group(1) == svc_name,
        'webhook dials Service %r but the Service is named %r' % (target.group(1), svc_name))
require(target.group(2) == svc_ns,
        'webhook dials namespace %r but the Service is in %r' % (target.group(2), svc_ns))
require(inject == '%s/%s' % (cert_ns, cert_name),
        'CA injection names %r but the Certificate is %s/%s' % (inject, cert_ns, cert_name))
require('%s.%s.svc' % (svc_name, svc_ns) in cert_dns,
        'Certificate covers %s, not the Service FQDN %s.%s.svc' % (cert_dns, svc_name, svc_ns))
require(cert_secret == dep_secret,
        'Certificate writes secret %r but the Deployment mounts %r' % (cert_secret, dep_secret))
require('--webhook-cert-path=' in dep,
        'Deployment does not pass --webhook-cert-path, so the manager starts with validation disabled')
# The webhook patch must ADD to args, not replace them. containers[].args is a list of plain strings with no
# merge key, so a strategic-merge patch silently drops every flag the base and the other overlays set. The
# first version did exactly that and deleted --metrics-bind-address: the rollout was clean, the webhook
# worked, and the operator exposed no metrics at all until Prometheus failed to connect to a port nothing
# was listening on.
require('--metrics-bind-address=' in dep,
        'Deployment lost --metrics-bind-address; the webhook patch replaced the args list instead of adding to it')
require('--health-probe-bind-address=' in dep,
        'Deployment lost --health-probe-bind-address; same cause as above')
require('DELETE' not in wh,
        'webhook intercepts DELETE; teardown belongs to the finalizer')

if failures:
    for f in failures:
        print('FAIL: %s' % f)
    sys.exit(1)
print('webhook overlay cross-references resolve')
PY
