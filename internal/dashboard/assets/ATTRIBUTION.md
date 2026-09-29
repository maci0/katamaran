# Vendored dashboard assets

The dashboard serves these files from its own origin. Nothing is fetched from a
CDN at runtime, so a cluster with no egress still renders, and no third-party
origin can inject script into the dashboard page.

Each file below is an unmodified upstream artifact. `SHA256SUMS` pins the exact
bytes that ship; `TestVendoredAssetsMatchSHA256SUMS` in
`internal/dashboard/assets_test.go` fails when a file's content drifts from its
recorded digest or when a new file lands here without a digest.

## Chart.js 4.5.1

- Files: `chart-4.5.1.min.js`, `chart-4.5.1.LICENSE.txt`
- Upstream: <https://github.com/chartjs/Chart.js> at tag `v4.5.1`
- License: MIT (see `chart-4.5.1.LICENSE.txt`, which also carries the notice
  for the bundled `@kurkle/color` v0.3.2, MIT)
- Used for: the latency chart. Loaded on demand, not at page load.

## Tailwind CSS 3.4.17 (standalone browser build)

- Files: `tailwind-3.4.17.js`, `tailwind-3.4.17.LICENSE.txt`
- Upstream: <https://github.com/tailwindlabs/tailwindcss> at tag `v3.4.17`
- License: MIT (see `tailwind-3.4.17.LICENSE.txt`, which also carries the
  notices for the bundled `jonschlinkert` and `micromatch` packages, MIT)
- Used for: generating the dashboard's CSS in the browser from the classes in
  `index.html`.

The Tailwind bundle is a build tool that runs in the page, which is why the
dashboard ships 400 KB of it instead of a static stylesheet: every edit to
`index.html` renders correctly with no separate CSS build step and no second
copy of the class list to keep in sync. The trade is deliberate.

## Updating a vendored asset

1. Take the new version's upstream artifact. Do not edit it.
2. Replace the file here under its versioned name, and carry the matching
   license notice over from the upstream license file.
3. `cd internal/dashboard/assets && sha256sum *.LICENSE.txt *.js *.min.js > SHA256SUMS`
4. Add a section above, and update the filename references in
   `internal/dashboard/server.go` (`newMux`) and `internal/dashboard/index.html`.
   Asset filenames carry their version, so serving is cacheable forever and a
   bump has to change the URL.
