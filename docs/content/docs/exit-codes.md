+++
title = 'Exit codes'
weight = 9
+++

| Code | Meaning |
|---|---|
| 0 | success, including "nothing to do" |
| 1 | unexpected failure |
| 2 | usage, config, or precondition error |
| 3 | API or authentication failure |
| 4 | conflict: a lease is held, the head moved, or a fork was found |
| 5 | integrity failure: checksum mismatch or a missing pack |
| 6 | partial success: some files failed, the rest are complete |
| 130 | interrupted |

`--output json` writes one JSON object per line to stdout, with human output on stderr,
so it composes in CI without either stream fighting the other.
