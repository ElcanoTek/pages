## What

<!-- One or two sentences: what changes and why. -->

## Checklist

- [ ] `make test` passes with the database suites running (`make test-db` first), and `gofmt`, `go vet` and `make lint` are clean
- [ ] `node --test test/frontend.test.js` and `npx playwright test` pass, or the PR says why they were not run
- [ ] Nothing renders agent HTML on the dashboard origin; `allow-same-origin` never joins `allow-scripts`
- [ ] Docs updated where behaviour changed
- [ ] New source files carry the SPDX header
- [ ] No secrets, real customer data, or internal hostnames in the diff

## Screenshots

<!-- For UI changes. -->
