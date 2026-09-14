#!/usr/bin/env python3
"""Draw the DCGM series a session captured, as SVG, with no dependencies.

The session keeps every DCGM_FI_DEV_GPU_UTIL line it sampled. This turns them into a picture so a reader can
see what the cards did, rather than only the two numbers a comparison reduces them to.

SVG rather than PNG because nothing here may invent a pixel: the output is text, every value in it can be
read back out of the file, and it diffs. There is also no matplotlib on the machines this repository is
developed on, and adding one to draw four lines would be the larger change.

It refuses rather than draws when the evidence is not there. An empty plot and a plot of a card that was
genuinely idle look identical, and only one of them is a measurement.
"""

import argparse
import gzip
import html
import pathlib
import re
import shutil
import subprocess
import sys
from collections import defaultdict

# DCGM's exposition line, of which this reads the labels that make a number attributable.
#
# gpu= is which card. pod= and namespace= are what the kubelet told the exporter, and they are the reason
# the raw lines are kept rather than only a per-card average: a utilisation figure with no owner cannot
# answer the question this session exists to ask.
LINE = re.compile(
    r'^DCGM_FI_DEV_GPU_UTIL\{(?P<labels>[^}]*)\}\s+(?P<value>[0-9.]+)\s*$'
)
LABEL = re.compile(r'(\w+)="([^"]*)"')

PALETTE = ["#2f6f9f", "#c1633c", "#4c9a5a", "#8a5fa8", "#b0893a", "#5f7d8c"]


def die(msg):
    print(f"FAIL: {msg}", file=sys.stderr)
    sys.exit(1)


def read_series(path):
    """Return {gpu: [(t, value, pod)]} and the set of pods seen, from a sampled .tsv or .tsv.gz."""
    opener = gzip.open if str(path).endswith(".gz") else open
    series = defaultdict(list)
    pods = set()
    failed = 0
    total = 0
    with opener(path, "rt") as fh:
        for raw in fh:
            raw = raw.rstrip("\n")
            if not raw:
                continue
            ts, _, rest = raw.partition("\t")
            if rest == "SCRAPE_FAILED":
                failed += 1
                continue
            m = LINE.match(rest)
            if not m:
                continue
            total += 1
            labels = dict(LABEL.findall(m.group("labels")))
            gpu = labels.get("gpu", "?")
            pod = labels.get("pod", "")
            if pod:
                pods.add(pod)
            series[gpu].append((int(ts), float(m.group("value")), pod))
    return series, pods, failed, total


def svg_escape(s):
    return html.escape(str(s), quote=True)


def render(series, pods, failed, out_path, title):
    gpus = sorted(series, key=lambda g: (len(g), g))
    t0 = min(p[0] for pts in series.values() for p in pts)
    t1 = max(p[0] for pts in series.values() for p in pts)
    span = max(t1 - t0, 1)

    W, H = 960, 120 * len(gpus) + 124
    L, R, TOP = 64, 24, 96
    plot_w = W - L - R
    row_h = 90
    gap = 30

    parts = [
        f'<svg xmlns="http://www.w3.org/2000/svg" width="{W}" height="{H}" '
        f'viewBox="0 0 {W} {H}" font-family="ui-monospace,SFMono-Regular,Menlo,monospace" font-size="11">',
        f'<rect width="{W}" height="{H}" fill="#fbfbfa"/>',
        f'<text x="{L}" y="24" font-size="14" fill="#1a1a1a">{svg_escape(title)}</text>',
        f'<text x="{L}" y="41" fill="#555">{span}s sampled, {len(gpus)} card(s), '
        f'{len(pods)} pod(s) named by the exporter'
        + (f', {failed} failed scrape(s)' if failed else '')
        + '</text>',
    ]

    # A snapshot drawn as a time series is a lie the axes tell for you.
    #
    # A session that died before its study ran left one scrape: every card had a single sample, at zero,
    # and the drawing showed four flat empty panels that read exactly like four cards idling through a run
    # that happened. Nothing in the picture said the run had not happened. The banner says it.
    most = max(len(pts) for pts in series.values())
    if most < 3:
        parts.append(
            f'<rect x="{L}" y="50" width="{plot_w}" height="22" fill="#fdf3e3" stroke="#d9a441"/>'
            f'<text x="{L + 8}" y="65" fill="#8a5a00">'
            f'SNAPSHOT, NOT A SERIES &#8212; {most} sample(s) per card over {span}s. '
            'A flat line means nothing was watched.</text>'
        )

    for i, gpu in enumerate(gpus):
        top = TOP + i * (row_h + gap)
        base = top + row_h
        colour = PALETTE[i % len(PALETTE)]
        pts = sorted(series[gpu])

        parts.append(f'<rect x="{L}" y="{top}" width="{plot_w}" height="{row_h}" fill="#fff" stroke="#e2e2e0"/>')
        for frac, label in ((0.0, "100"), (0.5, "50"), (1.0, "0")):
            y = top + frac * row_h
            parts.append(f'<line x1="{L}" y1="{y:.1f}" x2="{L + plot_w}" y2="{y:.1f}" stroke="#eee"/>')
            parts.append(f'<text x="{L - 8}" y="{y + 3:.1f}" text-anchor="end" fill="#888">{label}</text>')

        coords = []
        for t, v, _pod in pts:
            x = L + (t - t0) / span * plot_w
            y = base - (min(v, 100.0) / 100.0) * row_h
            coords.append(f"{x:.2f},{y:.2f}")
        parts.append(
            f'<polyline points="{" ".join(coords)}" fill="none" stroke="{colour}" stroke-width="1.4"/>'
        )

        busy = sum(1 for _t, v, _p in pts if v > 0)
        owners = sorted({p for _t, _v, p in pts if p})
        owner_text = ", ".join(owners) if owners else "no pod named on any sample"
        parts.append(
            f'<text x="{L}" y="{top - 6}" fill="#1a1a1a">gpu {svg_escape(gpu)} '
            f'&#183; {busy}/{len(pts)} samples above zero &#183; {svg_escape(owner_text)}</text>'
        )

    parts.append(
        f'<text x="{L}" y="{H - 12}" fill="#777">'
        'DCGM_FI_DEV_GPU_UTIL as the exporter reported it. Percent of sampled time the card had work, '
        'not a share of any budget.</text>'
    )
    parts.append("</svg>")
    out_path.write_text("\n".join(parts))


# The SVG is the artifact; a PNG is a convenience for pasting into a review.
#
# Rasterised by whichever headless browser is on the machine rather than by an imaging library, because
# there is no imaging library here and the browser is already the thing that agrees with how the SVG will be
# read. A failure to rasterise is reported, never swallowed: a stale PNG beside a fresh SVG is exactly the
# kind of quiet disagreement this repository spends its comments on.
W_HINT = 980


def rasterise(svg, png, width, height):
    for browser in ("google-chrome", "chromium", "chromium-browser"):
        exe = shutil.which(browser)
        if not exe:
            continue
        png.unlink(missing_ok=True)
        subprocess.run(
            [exe, "--headless", "--disable-gpu", "--no-sandbox", "--hide-scrollbars",
             f"--screenshot={png}", f"--window-size={width},{height}", svg.resolve().as_uri()],
            capture_output=True, timeout=120,
        )
        if png.is_file() and png.stat().st_size > 0:
            return True
    return False


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("series", help="device-util.tsv or device-util.tsv.gz from a session")
    ap.add_argument("-o", "--out", help="output .svg (default: alongside the input)")
    ap.add_argument("--title", default="device observation")
    ap.add_argument("--png", action="store_true",
                    help="also write a PNG, rasterised by a headless browser; the SVG stays the original")
    args = ap.parse_args()

    src = pathlib.Path(args.series)
    if not src.is_file():
        die(f"{src} does not exist; a session that captured no series has nothing to draw")

    series, pods, failed, total = read_series(src)
    if not series:
        die(
            f"{src} holds no readable DCGM_FI_DEV_GPU_UTIL sample "
            f"({failed} scrape(s) recorded as failed). An empty plot and an idle card look the same, "
            "so nothing is drawn."
        )

    out = pathlib.Path(args.out) if args.out else src.with_suffix("").with_suffix(".svg")
    render(series, pods, failed, out, args.title)
    print(f"{out}: {total} samples across {len(series)} card(s), {len(pods)} pod(s) named, {failed} failed scrape(s)")

    if args.png:
        png = out.with_suffix(".png")
        if not rasterise(out, png, W_HINT, 120 * len(series) + 130):
            die(f"could not rasterise {out}; the SVG is written and readable, the PNG is not")
        print(f"{png}: rasterised")


if __name__ == "__main__":
    main()
