"""Collapse a repeating block of transcript lines into one copy plus a count.

A timeout scenario polls 120 times, which is 360 near-identical lines in a golden file that a human is
supposed to read a diff of. The count is behaviour and must not be thrown away, so the block is kept
once and annotated rather than truncated: changing the poll interval, the polled key, or the number of
attempts all still show up as a diff.
"""

import sys

MAX_BLOCK = 8
MIN_REPEATS = 3


def collapse(lines):
    out, i = [], 0
    while i < len(lines):
        best = None
        for k in range(1, MAX_BLOCK + 1):
            block = lines[i:i + k]
            if len(block) < k:
                break
            n = 0
            while lines[i + n * k:i + (n + 1) * k] == block:
                n += 1
            if n >= MIN_REPEATS and (best is None or n * k > best[0] * best[1]):
                best = (n, k)
        if best:
            n, k = best
            out.extend(lines[i:i + k])
            out.append(f"--- the {k} line(s) above repeat {n} times ---")
            i += n * k
        else:
            out.append(lines[i])
            i += 1
    return out


if __name__ == "__main__":
    sys.stdout.write("\n".join(collapse(sys.stdin.read().splitlines())) + "\n")
