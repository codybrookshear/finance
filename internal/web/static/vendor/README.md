# Vendored browser libraries

Served from the app itself (no CDNs). Each file was extracted from the npm
tarball after checking the tarball against the registry's published sha512
integrity. `TestVendoredAssets` fails if a file changes; to upgrade, repeat
that process and update the table and the test together.

| file | package | sha256 |
|---|---|---|
| `htmx.min.js` | htmx.org@2.0.10 | `71ea67185bfa8c98c39d31717c6fce5d852370fcdfd129db4543774d3145c0de` |
| `htmx.LICENSE` | htmx.org@2.0.10 | `d3d2456f76414f2456104660ebd65aff1c04cd7966b942bdabd63f3cdb316a38` |
| `uPlot.iife.min.js` | uplot@1.6.32 | `19c8d4c6ad88929a79f4ae49d6f7161566dfd0ba3d15cc495e974f787eb78f1f` |
| `uPlot.min.css` | uplot@1.6.32 | `df630c6a8d6f8eeaff264b50f73ce5b114f646ffd9a0bb74f049b0a00135fa04` |
| `uPlot.LICENSE` | uplot@1.6.32 | `8f989229699b4fe2f1a0432d0e9edc338a8a911e250e2d1b01ecd770a5f5b1bd` |

| package | tarball | npm integrity |
|---|---|---|
| htmx.org@2.0.10 | https://registry.npmjs.org/htmx.org/-/htmx.org-2.0.10.tgz | `sha512-kdeJe7ZVwaS6QMz/ebBIVtZdpwen6L0OQ5GOhPV9MKBb196TCZeZu4yA7ZIQsaLKv7EpXz+So7KSXNuHXhj7Cw==` |
| uplot@1.6.32 | https://registry.npmjs.org/uplot/-/uplot-1.6.32.tgz | `sha512-KIMVnG68zvu5XXUbC4LQEPnhwOxBuLyW1AHtpm6IKTXImkbLgkMy+jabjLgSLMasNuGGzQm/ep3tOkyTxpiQIw==` |

htmx is 0BSD and uPlot is MIT; their licenses are alongside.
