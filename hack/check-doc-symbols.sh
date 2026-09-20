#!/usr/bin/env bash
# Checks that every name the published docs put in backticks actually exists in this repository.
#
# The docs are the only part of this project nothing verified. Code has gofmt, vet, golangci-lint, envtest,
# e2e and actionlint; prose had nothing, and a claim-by-claim audit on 2026-09-19 found 54 statements that
# the repository contradicts. Fourteen of them were a name: `ValidatingAdmissionPolicy`, `QuotaSatisfied`,
# `NodeClassHealthy`, `WarmCacheReady`, `TrainingQuotaSynced`, `admission_guard_decisions_total`,
# `unknown_api_key`, `tenant_not_provisioned`, `gpu.yml` and the rest. Every one is a string the docs spell
# out and the repository never mentions, which is exactly the kind of error a machine can see and a reader
# cannot -- a plausible name reads as true.
#
# What this does NOT check is whether a name that exists is described correctly. That half stays human. The
# point is to spend no more human attention on the half that does not need it.
#
# Why only docs/0*.md, docs/10*.md and README.md: the specs under docs/superpowers/specs/ are dated records
# of what was believed on the day they were written. A spec diverging from today's code is the archive
# working, not a defect, and counting it would drown the signal.
set -euo pipefail

cd "$(dirname "$0")/.."

ALLOW=${ALLOW:-hack/doc-symbols-allow.txt}
SELF_TEST=${1:-}

python3 - "$ALLOW" "$SELF_TEST" <<'PY'
import os, re, subprocess, sys, glob

allow_path, self_test = sys.argv[1], sys.argv[2]

# --- the allowlist -------------------------------------------------------------------------------------
# TOKEN<TAB>reason. The reason is mandatory: an allowlist whose entries carry no reason becomes a place to
# put anything that fails, and then the check passes because it was taught to.
allow = {}
if os.path.exists(allow_path):
    for n, line in enumerate(open(allow_path, encoding='utf-8'), 1):
        line = line.rstrip('\n')
        if not line.strip() or line.lstrip().startswith('#'):
            continue
        if '\t' not in line:
            sys.exit(f"{allow_path}:{n}: allowlist entry has no reason (use TOKEN<TAB>why it cannot be checked)")
        tok, reason = line.split('\t', 1)
        if not reason.strip():
            sys.exit(f"{allow_path}:{n}: allowlist entry has an empty reason")
        allow[tok.strip()] = reason.strip()

def tracked(*patterns):
    out = subprocess.run(['git', 'ls-files', '--'] + list(patterns), capture_output=True, text=True).stdout
    return [p for p in out.split('\n') if p]

docs = [d for d in tracked('docs/0*.md', 'docs/10*.md', 'README.md') if '/captures/' not in d]
if not docs:
    sys.exit("no documents matched; refusing to report success on an empty scan")

# --- collect every backticked token, with where it was said ---------------------------------------------
where = {}
for d in docs:
    text = open(d, encoding='utf-8').read()
    # Fenced blocks are examples and target designs, not claims about what exists.
    text = re.sub(r'```.*?```', lambda m: '\n' * m.group(0).count('\n'), text, flags=re.S)
    for ln, line in enumerate(text.split('\n'), 1):
        for tok in re.findall(r'`([^`\n]{2,80})`', line):
            where.setdefault(tok.strip(), []).append(f"{d}:{ln}")

# --- one blob of everything the repository actually says ------------------------------------------------
blob = []
for f in tracked('*.go', '*.yaml', '*.yml', '*.tf', '*.sh', '*.json', 'Makefile', '*.mod'):
    try:
        blob.append(open(f, encoding='utf-8', errors='ignore').read())
    except OSError:
        pass
blob = '\n'.join(blob)
if len(blob) < 10000:
    sys.exit("source blob is implausibly small; refusing to report success on a scan that read nothing")

TOPLEVEL = ('internal/', 'config/', 'hack/', 'docs/', 'cmd/', 'api/', 'infra/', 'test/',
            'scripts/', 'tools/', '.github/', 'bin/')
REPO_EXT = r'(ya?ml|go|sh|md|tf|json|mod|log)'

def classify(tok):
    """Return the class of token, or None when it is not something this check can judge."""
    if re.fullmatch(r'[A-Z][A-Za-z0-9]*(\.[A-Za-z][A-Za-z0-9]*)?', tok) and len(tok) > 3:
        return 'name'
    if re.fullmatch(r'[a-z][a-z0-9]*(_[a-z0-9]+){2,}', tok):
        return 'name'
    stripped = re.sub(r':\d+(-\d+)?$', '', tok)          # docs cite `file.go:120-135`
    if ' ' in stripped or stripped.startswith(('http', '$', '/')):
        return None
    if stripped.startswith(TOPLEVEL):
        return 'path'
    if re.fullmatch(rf'[A-Za-z0-9_][A-Za-z0-9_.-]*\.{REPO_EXT}', stripped):
        return 'path'
    return None

def path_exists(tok):
    p = re.sub(r':\d+(-\d+)?$', '', tok).rstrip('/')
    if os.path.exists(p):
        return True
    if glob.glob(p + '*'):                                # docs write `docs/05` for a numbered document
        return True
    return bool(tracked('*' + os.path.basename(p)))

def name_present(tok):
    # Docs cite a method as `Type.Method`, but Go writes `func (p Type) Method()`, so the dotted string
    # appears nowhere. Checking it verbatim reported `SharingPlan.Validate` and `RunManifest.GatewaySHA` as
    # absent when both are declared -- the check inventing the very kind of false claim it exists to catch.
    return all(part in blob for part in tok.split('.'))

missing = []
for tok, sites in sorted(where.items()):
    if tok in allow:
        continue
    kind = classify(tok)
    if kind is None:
        continue
    ok = path_exists(tok) if kind == 'path' else name_present(tok)
    if not ok:
        missing.append((kind, tok, sites))

# --- deliberately break it, so a pass means the detector can still see a failure -------------------------
if self_test == '--self-test':
    probe = 'ThisSymbolIsNotInTheRepositoryOnPurpose'
    if classify(probe) != 'name':
        sys.exit("SELF-TEST FAILED: the probe is not even classified as a name")
    if probe in blob:
        sys.exit("SELF-TEST FAILED: the probe string is somehow present in the source")
    print("self-test: an absent name is classified and would be reported -- the detector can fail")
    sys.exit(0)

if missing:
    print(f"{len(missing)} name(s) the docs spell out and this repository never mentions:\n")
    for kind, tok, sites in missing:
        print(f"  [{kind}] {tok}")
        for s in sites[:4]:
            print(f"        {s}")
    print(f"\nEither fix the document, or add the token to {allow_path} with a reason.")
    sys.exit(1)

print(f"checked {len(docs)} documents, {len(where)} backticked tokens, {len(allow)} allowed: all names resolve")
PY
